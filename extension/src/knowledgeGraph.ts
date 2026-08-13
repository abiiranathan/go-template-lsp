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
import { normalizeDictArg } from './scopeUtils';
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
    private workspaceRoot: string;
    private outputChannel: vscode.OutputChannel;
    private readonly statusBarItem: vscode.StatusBarItem;
    private partialContextCache = new Map<string, TemplateContext | null>();

    constructor(
        workspaceRoot: string,
        outputChannel: vscode.OutputChannel,
        statusBarItem: vscode.StatusBarItem,
    ) {
        this.workspaceRoot = workspaceRoot;
        this.outputChannel = outputChannel;
        this.statusBarItem = statusBarItem;
    }

    updateStatus(message: string): void {
        this.statusBarItem.text = `$(sync~spin) ${message}`;
        this.statusBarItem.show();
        this.outputChannel.appendLine(`[KnowledgeGraph] ${message}`);
    }

    clearStatus(): void {
        this.statusBarItem.hide();
    }

    getRelativeGoFile(absolutePath: string): string | null {
        const sourceDir: string = config.sourceDir();
        const sourceDirAbs = path.resolve(this.workspaceRoot, sourceDir);
        const rel = path.relative(sourceDirAbs, absolutePath);
        if (rel.startsWith('..') || path.isAbsolute(rel)) return null;
        return rel.replace(/\\/g, '/');
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

    private getParsedNodes(absolutePath: string): TemplateNode[] | undefined {
        if (this.astCache.has(absolutePath)) {
            return this.astCache.get(absolutePath);
        }

        let content: string;
        try {
            const openDoc = vscode.workspace.textDocuments.find(d => d.uri.fsPath === absolutePath);
            content = openDoc ? openDoc.getText() : fs.readFileSync(absolutePath, 'utf8');
        } catch (err) {
            this.outputChannel.appendLine(`[KnowledgeGraph] Failed to read file ${absolutePath}: ${err}`);
            return undefined;
        }

        try {
            const parser = new TemplateParser();
            const nodes = parser.parse(content);
            this.astCache.set(absolutePath, nodes);
            return nodes;
        } catch (err) {
            this.outputChannel.appendLine(`[KnowledgeGraph] Parse failed for ${absolutePath}: ${err}`);
            return undefined;
        }
    }

    private invalidateAstCache(absolutePath: string): void {
        this.astCache.delete(absolutePath);
    }

    build(analysisResult: AnalysisResult): KnowledgeGraph {
        this.partialContextCache.clear();
        this.astCache.clear();

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

        // ── Build named blocks registry directly from Go results ────────────────
        // Optimization: Go's validator already parsed and registered all named blocks concurrently.
        // We do NOT need to walk disk and regex-match files in Node.js again.
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

        this.graph = { templates, namedBlocks, namedBlockErrors, analyzedAt: new Date(), funcMaps, typeRegistry };

        this.outputChannel.appendLine(
            `[KnowledgeGraph] Built graph with ${templates.size} templates, ` +
            `${namedBlocks.size} named block(s), ` +
            `${funcMaps.size} template functions, ` +
            `${typeRegistry.size} registered type(s)`
        );

        return this.graph;
    }

    private createNamedBlockEntry(base: any, name: string): NamedBlockEntry {
        return {
            ...base,
            name,
            get node() {
                if (!(this as any)._node) {
                    const nodes = knowledgeGraphBuilder.getParsedNodes(this.absolutePath);
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
        const templateBase = this.getTemplateBase();
        let rel = path.relative(templateBase, absolutePath).replace(/\\/g, '/');

        if (this.graph.templates.has(rel)) {
            return this.graph.templates.get(rel);
        }

        const normalizedAbsPath = path.normalize(absolutePath).toLowerCase();
        for (const [tplPath, ctx] of this.graph.templates) {
            if (ctx.absolutePath && path.normalize(ctx.absolutePath).toLowerCase() === normalizedAbsPath) {
                return ctx;
            }
            if (rel.endsWith(tplPath) || tplPath.endsWith(rel)) {
                return ctx;
            }
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
        if (this.partialContextCache.has(absolutePath)) {
            return this.partialContextCache.get(absolutePath) ?? undefined;
        }

        const result = await this._findContextForFileAsPartialUncached(absolutePath);
        this.partialContextCache.set(absolutePath, result ?? null);
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

        const parentEntries = [...this.graph.templates.entries()].filter(
            ([, ctx]) => ctx.absolutePath && fs.existsSync(ctx.absolutePath)
        );

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
            return typeIndex.get(bare) ?? this.graph.typeRegistry.get(bare);
        };
    }

    resolveGoFilePath(relativeFile: string): string | null {
        const config = vscode.workspace.getConfiguration('rex');
        const sourceDir: string = config.get('sourceDir') ?? '.';
        const sourceDirAbs = path.resolve(this.workspaceRoot, sourceDir);
        const abs = path.join(sourceDirAbs, relativeFile);
        return fs.existsSync(abs) ? abs : null;
    }

    resolveTemplatePath(templatePath: string): string | null {
        const ctx = this.graph.templates.get(templatePath);
        if (ctx?.absolutePath && fs.existsSync(ctx.absolutePath)) {
            return ctx.absolutePath;
        }

        for (const [tplPath, tplCtx] of this.graph.templates) {
            if (tplPath.endsWith(templatePath) || templatePath.endsWith(tplPath)) {
                if (tplCtx.absolutePath && fs.existsSync(tplCtx.absolutePath)) {
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
            if (fs.existsSync(candidate)) {
                return candidate;
            }
        }

        return null;
    }

    updateTemplateFile(absolutePath: string, content: string) {
        this.invalidateAstCache(absolutePath);
        this.partialContextCache.clear();
    }
}

// ── Helpers ───────────────────────────────────────────────────────────────────

function fieldInfoToTemplateVar(f: FieldInfo): TemplateVar {
    return {
        name: f.name,
        type: f.type,
        fields: f.fields,
        isSlice: f.isSlice,
        defFile: f.defFile,
        defLine: f.defLine,
        defCol: f.defCol,
        doc: f.doc,
    };
}

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
        const childVisited = bare ? new Set([...visited, bare]) : visited;
        const childFields = f.fields && f.fields.length > 0
            ? f.fields
            : typeRegistry?.get(bare) ?? [];
        const d = 1 + maxFieldDepth(childFields, typeRegistry, childVisited);
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
    }
    return undefined;
}

let knowledgeGraphBuilder: KnowledgeGraphBuilder;

export function setKnowledgeGraphBuilder(builder: KnowledgeGraphBuilder) {
    knowledgeGraphBuilder = builder;
}
