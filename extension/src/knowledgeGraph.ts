import * as path from 'path';
import * as fs from 'fs';
import * as vscode from 'vscode';
import {
    AnalysisResult,
    KnowledgeGraph,
    NamedBlockRegistry,
    NamedBlockEntry,
    NamedBlockDuplicateError,
    TemplateContext,
    TemplateVar,
    TemplateNode,
    FieldInfo,
    FuncMapInfo,
    RenderCall,
    extractBareType,
} from './types';
import { TemplateParser, resolvePath } from './templateParser';
import { normalizeDictArg, fieldInfoToTemplateVar, ScopeUtils, findOpenDocument, normalizeFsPath } from './scopeUtils';
import { inferExpressionType } from './compiler/expressionParser';
import { config } from './config';

export class KnowledgeGraphBuilder {
    private graph: KnowledgeGraph = {
        templates: new Map(),
        namedBlocks: new Map(),
        namedBlockErrors: [],
        analyzedAt: new Date(),
        funcMaps: new Map(),
        typeRegistry: new Map(),
    };

    private astCache = new Map<string, TemplateNode[]>();
    private absPathToContext = new Map<string, TemplateContext>();
    private partialToParents = new Map<string, Set<string>>();
    private workspaceRoot: string;
    private outputChannel: vscode.OutputChannel;
    private readonly statusBarItem: vscode.StatusBarItem;
    private partialContextCache = new Map<string, TemplateContext | null>();

    /**
     * Memoized file-existence results, cleared on every graph rebuild and on
     * incremental template updates. Avoids per-request existsSync storms in
     * resolveTemplatePath / partial-parent fallback scans.
     */
    private existenceCache = new Map<string, boolean>();

    constructor(
        workspaceRoot: string,
        outputChannel: vscode.OutputChannel,
        statusBarItem: vscode.StatusBarItem,
    ) {
        this.workspaceRoot = workspaceRoot;
        this.outputChannel = outputChannel;
        this.statusBarItem = statusBarItem;
    }

    getRelativeGoFile(absolutePath: string): string | null {
        const sourceDir: string = config.sourceDir();
        const sourceDirAbs = path.resolve(this.workspaceRoot, sourceDir);
        const rel = path.relative(sourceDirAbs, absolutePath);
        if (rel.startsWith('..') || path.isAbsolute(rel)) return null;
        return rel.replace(/\\/g, '/');
    }

    /**
     * Returns absolute paths of templates known (from the pre-built
     * partialToParents index) to invoke the given partial / named-block name.
     * Lets position-based lookups skip O(all-templates) AST walks in the
     * common case.
     */
    getParentCandidates(partialName: string): string[] {
        const result = new Set<string>();
        const direct = this.partialToParents.get(partialName);
        if (direct) {
            for (const p of direct) result.add(p);
        }
        const base = path.basename(partialName);
        if (base !== partialName) {
            const byBase = this.partialToParents.get(base);
            if (byBase) {
                for (const p of byBase) result.add(p);
            }
        }
        return [...result];
    }

    private getTemplateBase(): string {
        const sourceDir: string = config.sourceDir();
        const templateBaseDir: string = config.templateBaseDir();
        const templateRoot: string = config.templateRoot();

        const baseDir = templateBaseDir
            ? path.resolve(this.workspaceRoot, templateBaseDir)
            : path.resolve(this.workspaceRoot, sourceDir);

        return path.join(baseDir, templateRoot);
    }

    getParsedNodes(absolutePath: string): TemplateNode[] | undefined {
        if (this.astCache.has(absolutePath)) {
            return this.astCache.get(absolutePath);
        }

        let content: string;
        try {
            const openDoc = findOpenDocument(absolutePath);
            content = openDoc ? openDoc.getText() : fs.readFileSync(absolutePath, 'utf8');
        } catch (err) {
            return undefined;
        }

        try {
            const parser = new TemplateParser();
            const nodes = parser.parse(content);
            this.astCache.set(absolutePath, nodes);
            return nodes;
        } catch (err) {
            return undefined;
        }
    }

    private invalidateAstCache(absolutePath: string): void {
        this.astCache.delete(absolutePath);
    }

    build(analysisResult: AnalysisResult): KnowledgeGraph {
        this.partialContextCache.clear();
        this.astCache.clear();
        this.absPathToContext.clear();
        this.partialToParents.clear();
        this.existenceCache.clear();
        TemplateParser.clearCache();
        // ScopeUtils keeps its own cross-request cache (named-block call
        // contexts). It must be invalidated here too, otherwise hover /
        // completion / definition inside named blocks keep serving types from
        // the previous graph until the window reloads.
        ScopeUtils.clearAllCaches();

        const templates = new Map<string, TemplateContext>();
        const templateBase = this.getTemplateBase();

        // ── Load global type registry from Go analyzer ─────────────────────────
        const typeRegistry = new Map<string, FieldInfo[]>();
        if (analysisResult.types) {
            for (const [typeName, fields] of Object.entries(analysisResult.types)) {
                typeRegistry.set(typeName, fields);
            }
        }

        const mergeRenderCall = (logicalPath: string, absPath: string, rc: RenderCall) => {
            let ctx = templates.get(logicalPath);
            if (!ctx) {
                ctx = {
                    templatePath: logicalPath,
                    absolutePath: absPath,
                    vars: new Map(),
                    renderCalls: [],
                };
                templates.set(logicalPath, ctx);
            }

            ctx.renderCalls.push(rc);

            for (const v of rc.vars ?? []) {
                const existing = ctx.vars.get(v.name);
                if (!existing || isMoreComplete(v, existing, typeRegistry)) {
                    ctx.vars.set(v.name, hydrateVar(v, typeRegistry));
                }
            }
        };

        // ── Process render calls ────────────────────────────────────────────────
        for (const rc of analysisResult.renderCalls ?? []) {
            const logicalPath = rc.template.replace(/^\.\//, '');
            const absPath = path.join(templateBase, logicalPath);

            mergeRenderCall(logicalPath, absPath, rc);

            if (analysisResult.namedBlocks && analysisResult.namedBlocks[logicalPath]) {
                const entries = analysisResult.namedBlocks[logicalPath];
                if (entries.length > 0) {
                    const entry = entries[0];
                    mergeRenderCall(entry.templatePath, entry.absolutePath, rc);
                }
            }
        }

        for (const ctx of templates.values()) {
            if (ctx.absolutePath) {
                this.absPathToContext.set(path.normalize(ctx.absolutePath).toLowerCase(), ctx);
            }
        }

        // ── Build named blocks registry directly from Go results ────────────────
        const namedBlocks: NamedBlockRegistry = new Map();
        if (analysisResult.namedBlocks) {
            for (const [name, entries] of Object.entries(analysisResult.namedBlocks)) {
                const fullEntries: NamedBlockEntry[] = entries.map(e => this.createNamedBlockEntry(e, name));
                namedBlocks.set(name, fullEntries);
            }
        }

        const namedBlockErrors = analysisResult.namedBlockErrors ?? [];

        const funcMaps = new Map<string, FuncMapInfo>();
        for (const fm of analysisResult.funcMaps ?? []) {
            funcMaps.set(fm.name, fm);
        }

        // ── Pre-build global type index ─────────────────────────────────────────
        const globalTypeIndex = new Map<string, FieldInfo[]>();
        const indexVar = (v: TemplateVar | FieldInfo) => {
            let bare = extractBareType(v.type);
            let retFields: FieldInfo[] | undefined;

            if (v.type === 'method' && (v as FieldInfo).returns?.length) {
                bare = extractBareType((v as FieldInfo).returns![0].type);
                retFields = (v as FieldInfo).returns![0].fields;
            } else if (v.type.startsWith('func(')) {
                const match = v.type.match(/func\([^)]*\)\s*(.+)/);
                if (match && match[1]) {
                    let retType = match[1].trim();
                    if (retType.startsWith('(')) {
                        const commaIdx = retType.indexOf(',');
                        const endIdx = retType.indexOf(')');
                        const cutIdx = commaIdx !== -1 ? commaIdx : endIdx;
                        retType = retType.slice(1, cutIdx).trim();
                    }
                    bare = extractBareType(retType);
                }
            }

            const fieldsToIndex = retFields || v.fields;
            if (bare && bare !== 'method' && !bare.startsWith('func(') && fieldsToIndex && fieldsToIndex.length > 0) {
                const existing = globalTypeIndex.get(bare);
                if (existing) {
                    const existingNames = new Set(existing.map(f => f.name));
                    for (const f of fieldsToIndex) {
                        if (!existingNames.has(f.name)) {
                            existing.push(f);
                            indexVar(f);
                        }
                    }
                } else {
                    globalTypeIndex.set(bare, [...fieldsToIndex]);
                    for (const f of fieldsToIndex) indexVar(f);
                }
            } else if (fieldsToIndex && fieldsToIndex.length > 0) {
                for (const f of fieldsToIndex) indexVar(f);
            }
        };

        for (const [typeName, fields] of typeRegistry) {
            if (fields && fields.length > 0) {
                globalTypeIndex.set(typeName, fields);
                for (const f of fields) indexVar(f);
            }
        }

        for (const [, ctx] of templates) {
            for (const v of ctx.vars.values()) indexVar(v);
        }

        for (const [, fn] of funcMaps) {
            if (!fn.returnTypeFields || fn.returnTypeFields.length === 0) continue;
            const retType = fn.returns?.[0]?.type ?? '';
            const bare = extractBareType(retType);
            if (bare) {
                const existing = globalTypeIndex.get(bare);
                if (existing) {
                    const existingNames = new Set(existing.map(f => f.name));
                    for (const f of fn.returnTypeFields) {
                        if (!existingNames.has(f.name)) {
                            existing.push(f);
                            indexVar(f);
                        }
                    }
                } else {
                    globalTypeIndex.set(bare, [...fn.returnTypeFields]);
                    for (const f of fn.returnTypeFields) indexVar(f);
                }
            }
        }

        // ── Pre-index partial calls ─────────────────────────────────────────────
        for (const [, parentCtx] of templates) {
            if (!parentCtx.absolutePath) continue;
            const nodes = this.getParsedNodes(parentCtx.absolutePath);
            if (!nodes) continue;
            this.indexPartialCallsInNodes(nodes, parentCtx.absolutePath);
        }

        this.graph = { templates, namedBlocks, namedBlockErrors, analyzedAt: new Date(), funcMaps, typeRegistry, globalTypeIndex };

        this.outputChannel.appendLine(
            `[KnowledgeGraph] Built graph with ${templates.size} templates, ` +
            `${namedBlocks.size} named block(s), ` +
            `${funcMaps.size} template functions, ` +
            `${typeRegistry.size} registered type(s)`
        );

        return this.graph;
    }

    private indexPartialCallsInNodes(nodes: TemplateNode[], parentAbsPath: string) {
        for (const node of nodes) {
            if (node.kind === 'partial' && node.partialName) {
                let parentSet = this.partialToParents.get(node.partialName);
                if (!parentSet) {
                    parentSet = new Set<string>();
                    this.partialToParents.set(node.partialName, parentSet);
                }
                parentSet.add(parentAbsPath);
            } else if (node.kind === 'block' && node.blockName) {
                let parentSet = this.partialToParents.get(node.blockName);
                if (!parentSet) {
                    parentSet = new Set<string>();
                    this.partialToParents.set(node.blockName, parentSet);
                }
                parentSet.add(parentAbsPath);
            }
            if (node.children) this.indexPartialCallsInNodes(node.children, parentAbsPath);
            if (node.elseChildren) this.indexPartialCallsInNodes(node.elseChildren, parentAbsPath);
        }
    }

    private createNamedBlockEntry(base: any, name: string): NamedBlockEntry {
        return {
            ...base,
            name,
            get node() {
                if (!(this as any)._node) {
                    const nodes = knowledgeGraphBuilder?.getParsedNodes(this.absolutePath);
                    const found = nodes ? findBlockNode(nodes, name) : undefined;
                    (this as any)._node = found ?? {
                        kind: 'define',
                        path: [],
                        rawText: '',
                        line: this.line,
                        col: this.col,
                        blockName: name,
                    };
                }
                return (this as any)._node;
            }
        };
    }

    getGraph(): KnowledgeGraph {
        return this.graph;
    }

    lookupNamedBlock(name: string): NamedBlockEntry | undefined {
        const entries = this.graph.namedBlocks.get(name);
        if (!entries || entries.length === 0) return undefined;
        return entries[0];
    }

    getDuplicateErrorsForBlock(name: string): NamedBlockDuplicateError[] {
        return this.graph.namedBlockErrors.filter(e => e.name === name);
    }

    getAllDuplicateErrors(): NamedBlockDuplicateError[] {
        return this.graph.namedBlockErrors;
    }

    findContextForFile(absolutePath: string): TemplateContext | undefined {
        const norm = path.normalize(absolutePath).toLowerCase();
        const hit = this.absPathToContext.get(norm);
        if (hit) return hit;

        const templateBase = this.getTemplateBase();
        let rel = path.relative(templateBase, absolutePath).replace(/\\/g, '/');

        if (this.graph.templates.has(rel)) {
            return this.graph.templates.get(rel);
        }

        const base = path.basename(absolutePath);
        for (const [, ctx] of this.graph.templates) {
            if (path.basename(ctx.templatePath) === base) {
                return ctx;
            }
        }

        return undefined;
    }

    async findContextForFileAsPartialAsync(absolutePath: string): Promise<TemplateContext | undefined> {
        // Normalize the cache key so Windows casing variants share entries
        // instead of creating duplicate/stale ones.
        const cacheKey = normalizeFsPath(absolutePath);
        if (this.partialContextCache.has(cacheKey)) {
            return this.partialContextCache.get(cacheKey) ?? undefined;
        }

        const result = await this._findContextForFileAsPartialUncached(absolutePath);
        this.partialContextCache.set(cacheKey, result ?? null);
        return result;
    }

    private async _findContextForFileAsPartialUncached(absolutePath: string): Promise<TemplateContext | undefined> {
        const templateBase = this.getTemplateBase();
        const partialRelPath = path.relative(templateBase, absolutePath).replace(/\\/g, '/');

        // Priority 1: named block defined inside this file with variables
        for (const [blockName, entries] of this.graph.namedBlocks) {
            for (const entry of entries) {
                if (path.normalize(entry.absolutePath).toLowerCase() === path.normalize(absolutePath).toLowerCase()) {
                    const blockCtx = this.graph.templates.get(blockName);
                    if (blockCtx && blockCtx.vars.size > 0) {
                        return {
                            templatePath: partialRelPath,
                            absolutePath: absolutePath,
                            vars: blockCtx.vars,
                            renderCalls: blockCtx.renderCalls,
                        };
                    }
                }
            }
        }

        // Priority 2: Parent templates calling this file directly
        const partialBasename = path.basename(absolutePath);
        const normalizedTargetAbsPath = path.normalize(absolutePath).toLowerCase();
        const definedBlocksInFile = new Set<string>();
        for (const [blockName, entries] of this.graph.namedBlocks) {
            for (const entry of entries) {
                if (path.normalize(entry.absolutePath).toLowerCase() === normalizedTargetAbsPath) {
                    definedBlocksInFile.add(blockName);
                }
            }
        }

        // Collect candidate parent absolute paths using partialToParents index
        const candidateAbsPaths = new Set<string>();
        const addCandidates = (key: string) => {
            const parents = this.partialToParents.get(key);
            if (parents) {
                for (const p of parents) candidateAbsPaths.add(p);
            }
        };

        addCandidates(partialRelPath);
        addCandidates(partialBasename);
        for (const blockName of definedBlocksInFile) {
            addCandidates(blockName);
        }

        let parentEntries: Array<[string, TemplateContext]>;
        if (candidateAbsPaths.size > 0) {
            parentEntries = [];
            for (const abs of candidateAbsPaths) {
                const ctx = this.findContextForFile(abs);
                if (ctx) parentEntries.push([ctx.templatePath, ctx]);
            }
        } else {
            parentEntries = [...this.graph.templates.entries()].filter(
                ([, ctx]) => ctx.absolutePath && this.fileExistsCached(ctx.absolutePath)
            );
        }

        let foundAny = false;
        const mergedVars = new Map<string, TemplateVar>();
        const mergedRenderCalls: RenderCall[] = [];

        for (const [, parentCtx] of parentEntries) {
            const nodes = this.getParsedNodes(parentCtx.absolutePath!);
            if (!nodes) continue;

            const partialCall = this.findPartialCall(nodes, partialRelPath, partialBasename, definedBlocksInFile);
            if (!partialCall) continue;

            foundAny = true;
            const resolved = this.resolvePartialVars(
                partialCall.partialContext ?? '.',
                parentCtx.vars
            );

            for (const [k, v] of resolved.vars) {
                const existing = mergedVars.get(k);
                if (!existing || isMoreComplete(v, existing, this.graph.typeRegistry)) {
                    mergedVars.set(k, v);
                }
            }

            mergedRenderCalls.push(...parentCtx.renderCalls);
        }

        if (foundAny) {
            return {
                templatePath: partialRelPath,
                absolutePath: absolutePath,
                vars: mergedVars,
                renderCalls: mergedRenderCalls,
            };
        }

        return undefined;
    }

    private findPartialCall(
        nodes: TemplateNode[],
        partialRelPath: string,
        partialBasename: string,
        definedBlocksInFile?: Set<string>
    ): TemplateNode | undefined {
        for (const node of nodes) {
            if (node.kind === 'partial' && node.partialName) {
                const name = node.partialName;
                if (
                    name === partialRelPath ||
                    name === partialBasename ||
                    partialRelPath.endsWith('/' + name) ||
                    partialRelPath.endsWith(name) ||
                    (definedBlocksInFile && definedBlocksInFile.has(name))
                ) {
                    return node;
                }
            }

            if (node.children) {
                const found = this.findPartialCall(node.children, partialRelPath, partialBasename, definedBlocksInFile);
                if (found) return found;
            }
        }
        return undefined;
    }

    private resolvePartialVars(
        contextArg: string,
        vars: Map<string, TemplateVar>
    ): { vars: Map<string, TemplateVar> } {
        if (contextArg === '.' || contextArg === '$') {
            return { vars: new Map(vars) };
        }

        const normalizedCtx = normalizeDictArg(contextArg);

        if (normalizedCtx.startsWith('dict ')) {
            const fieldResolver = this.buildLocalFieldResolver(vars);
            const dictType = inferExpressionType(normalizedCtx, vars, [], undefined, undefined, fieldResolver);
            if (dictType && dictType.fields) {
                const partialVars = new Map<string, TemplateVar>();
                for (const f of dictType.fields) {
                    partialVars.set(f.name, {
                        name: f.name,
                        type: f.type,
                        fields: f.fields,
                        isSlice: f.isSlice ?? false,
                        isMap: f.isMap,
                        elemType: f.elemType,
                        keyType: f.keyType,
                    });
                }
                return { vars: partialVars };
            }
        }

        const parser = new TemplateParser();
        const parsedPath = parser.parseDotPath(normalizedCtx);
        const result = resolvePath(parsedPath, vars, [], undefined, this.buildLocalFieldResolver(vars));

        if (!result.found) {
            return { vars: new Map() };
        }

        const partialVars = new Map<string, TemplateVar>();
        if (result.fields) {
            for (const f of result.fields) {
                partialVars.set(f.name, fieldInfoToTemplateVar(f));
            }
        }

        return { vars: partialVars };
    }

    private buildLocalFieldResolver(
        vars: Map<string, TemplateVar>
    ): (typeStr: string) => FieldInfo[] | undefined {
        const typeIndex = new Map<string, FieldInfo[]>();

        const indexVar = (v: TemplateVar | FieldInfo) => {
            const bare = extractBareType(v.type);
            if (bare && v.fields && v.fields.length > 0 && !typeIndex.has(bare)) {
                typeIndex.set(bare, v.fields);
                for (const f of v.fields) indexVar(f);
            }
        };
        for (const v of vars.values()) indexVar(v);

        return (t: string) => {
            const bare = extractBareType(t);
            return typeIndex.get(bare) ??
                this.graph.globalTypeIndex?.get(bare) ??
                this.graph.typeRegistry.get(bare);
        };
    }

    resolveGoFilePath(relativeFile: string): string | null {
        // Use the central gotpl config (a previous version read the legacy
        // 'rex' namespace here, resolving definitions against the wrong root).
        const sourceDir: string = config.sourceDir();
        const sourceDirAbs = path.resolve(this.workspaceRoot, sourceDir);
        const abs = path.join(sourceDirAbs, relativeFile);
        return this.fileExistsCached(abs) ? abs : null;
    }

    resolveTemplatePath(templatePath: string): string | null {
        const ctx = this.graph.templates.get(templatePath);
        if (ctx?.absolutePath && this.fileExistsCached(ctx.absolutePath)) {
            return ctx.absolutePath;
        }

        for (const [tplPath, tplCtx] of this.graph.templates) {
            if (tplPath.endsWith(templatePath) || templatePath.endsWith(tplPath)) {
                if (tplCtx.absolutePath && this.fileExistsCached(tplCtx.absolutePath)) {
                    return tplCtx.absolutePath;
                }
            }
        }

        const templateBase = this.getTemplateBase();
        const candidates = [
            path.join(templateBase, templatePath),
            path.join(this.workspaceRoot, templatePath),
        ];

        for (const candidate of candidates) {
            if (this.fileExistsCached(candidate)) {
                return candidate;
            }
        }

        return null;
    }

    /** Memoized fs.existsSync; cache is dropped on rebuild/template update. */
    private fileExistsCached(absPath: string): boolean {
        let exists = this.existenceCache.get(absPath);
        if (exists === undefined) {
            try {
                exists = fs.existsSync(absPath);
            } catch {
                exists = false;
            }
            this.existenceCache.set(absPath, exists);
        }
        return exists;
    }

    updateTemplateFile(absolutePath: string, content: string) {
        this.invalidateAstCache(absolutePath);
        this.partialContextCache.clear();
        this.existenceCache.delete(absolutePath);
        this.absPathToContext.delete(path.normalize(absolutePath).toLowerCase());
        const ctx = this.findContextForFile(absolutePath);
        if (ctx && ctx.absolutePath) {
            this.absPathToContext.set(path.normalize(ctx.absolutePath).toLowerCase(), ctx);
        }
    }
}

// ── Helpers ───────────────────────────────────────────────────────────────────

function hydrateVar(v: TemplateVar, typeRegistry: Map<string, FieldInfo[]>): TemplateVar {
    if (v.fields && v.fields.length > 0) return v;
    const bare = extractBareType(v.type);
    const fields = typeRegistry.get(bare);
    if (fields && fields.length > 0) {
        return { ...v, fields };
    }
    return v;
}

function isMoreComplete(
    a: TemplateVar,
    b: TemplateVar,
    typeRegistry: Map<string, FieldInfo[]>
): boolean {
    const depthA = maxFieldDepth(a.fields ?? [], typeRegistry);
    const depthB = maxFieldDepth(b.fields ?? [], typeRegistry);
    return depthA > depthB;
}

function maxFieldDepth(
    fields: FieldInfo[],
    typeRegistry?: Map<string, FieldInfo[]>,
    visited: Set<string> = new Set()
): number {
    if (fields.length === 0) return 0;
    let max = 0;
    for (const f of fields) {
        const bare = extractBareType(f.type);
        if (bare && visited.has(bare)) continue;
        // Backtrack a shared visited set instead of allocating a copy per field.
        if (bare) visited.add(bare);
        const childFields = f.fields && f.fields.length > 0
            ? f.fields
            : typeRegistry?.get(bare) ?? [];
        const d = 1 + maxFieldDepth(childFields, typeRegistry, visited);
        if (bare) visited.delete(bare);
        if (d > max) max = d;
    }
    return max;
}

function findBlockNode(nodes: TemplateNode[], name: string): TemplateNode | undefined {
    for (const n of nodes) {
        if ((n.kind === 'define' || n.kind === 'block') && n.blockName === name) {
            return n;
        }
        if (n.children) {
            const found = findBlockNode(n.children, name);
            if (found) return found;
        }
        if (n.elseChildren) {
            const found = findBlockNode(n.elseChildren, name);
            if (found) return found;
        }
    }
    return undefined;
}

let knowledgeGraphBuilder: KnowledgeGraphBuilder | undefined;

export function setKnowledgeGraphBuilder(builder: KnowledgeGraphBuilder) {
    knowledgeGraphBuilder = builder;
}
