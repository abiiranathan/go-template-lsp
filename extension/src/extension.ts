/**
 * @file extension.ts
 * @description Main entry point and lifecycle coordinator for the Go Template LSP extension.
 *
 * This module manages:
 * 1. Extension lifecycle: Activation, initialization, configuration listening, and teardown.
 * 2. Background daemon integration: Spawning, syncing, and communicating with `gotpl-analyzer`.
 * 3. Language feature registration: Providers for Hover, Auto-completion, and Go-to-Definition.
 * 4. Diagnostics lifecycle: Splitting diagnostics into distinct collections to avoid race conditions
 *    and UI flicker between CLI builds, live daemon checks, and AST duplicate-block audits.
 * 5. Concurrency & Performance Guards: Strict debouncing, in-flight build locks, and precise file-save
 *    filters so heavy Go toolchain operations never starve `gopls`, formatters, or file I/O.
 */

import * as vscode from 'vscode';
import * as path from 'path';
import * as fs from 'fs';
import { GoAnalyzer } from './analyzer';
import { KnowledgeGraphBuilder, setKnowledgeGraphBuilder } from './knowledgeGraph';
import { TemplateValidator } from './validator';
import { KnowledgeGraphPanel } from './graphPanel';
import { KnowledgeGraph, GoValidationError, NamedBlockDuplicateError, RenderCall } from './types';

import { config } from './config';
import { AnalyzerInstaller } from './installer';

// ─── Constants ────────────────────────────────────────────────────────────────

/**
 * Document selector identifying files that should receive Go template language features.
 * Targets HTML, Go-Template language IDs, and .tmpl / .html file extensions.
 */
const TEMPLATE_SELECTOR: vscode.DocumentSelector = [
  { language: 'html', scheme: 'file' },
  { language: 'go-template', scheme: 'file' },
  { language: 'gotmpl', scheme: 'file' },
  { language: 'gohtml', scheme: 'file' },
  { pattern: '**/*.tmpl' },
  { pattern: '**/*.html' },
  { pattern: '**/*.gohtml' },
];

/**
 * Document selector for Go source files (used to provide Go-to-Definition from c.Render() calls).
 */
const GO_SELECTOR: vscode.DocumentSelector = [
  { language: 'go', scheme: 'file' }
];

/** Flag indicating whether the initial index has been executed. */
let isInitialized = false;

// ─── Module-Level State ───────────────────────────────────────────────────────

/**
 * Diagnostics produced by the batch Go CLI analyzer (`go/packages` AST inspection).
 * Persists project-wide errors across file edits until the next workspace rebuild.
 */
let analyzerCollection: vscode.DiagnosticCollection;

/**
 * Diagnostics produced in real-time by the persistent Go JSON-RPC daemon for currently active editors.
 * Re-evaluated on keystrokes to give instant feedback.
 */
let editorCollection: vscode.DiagnosticCollection;

/**
 * Diagnostics for project-wide structural errors (e.g. duplicate `{{ define "name" }}` blocks across files).
 * Rebuilt alongside the knowledge graph.
 */
let namedBlockCollection: vscode.DiagnosticCollection;

/** Output channel for extension logs and debugging information. */
let outputChannel: vscode.OutputChannel;

/** In-memory graph mapping Go render calls, passed structs, template partials, and blocks. */
let graphBuilder: KnowledgeGraphBuilder | undefined;

/** Façade providing validation, hover, definition, and completion logic. */
let validator: TemplateValidator | undefined;

/** Cached snapshot of the active KnowledgeGraph. */
let currentGraph: KnowledgeGraph | undefined;

/** Client interface communicating with the Go backend analyzer binary/daemon. */
let analyzer: GoAnalyzer | undefined;

/** Status bar indicator showing indexing progress, template counts, and errors. */
let statusBarItem: vscode.StatusBarItem;

// ─── Concurrency & Debounce State ─────────────────────────────────────────────

/**
 * Concurrency guard preventing multiple Go source analyses (`go/packages.Load`) from running concurrently.
 * Overlapping analyses cause lock contention in the Go build cache and freeze `gopls`.
 */
let isRebuilding = false;

/**
 * Stores the pending rebuild mode if a rebuild request is triggered while an existing one is in flight.
 * If multiple requests queue up, 'full' takes precedence over 'templates-only'.
 */
let pendingRebuildMode: 'full' | 'templates-only' | null = null;

/** Timer handle for debouncing workspace-wide index rebuilds. */
let rebuildTimer: NodeJS.Timeout | undefined;

/** Timer handle for debouncing cross-validation across all currently open template editors. */
let validateOpenTemplatesTimer: NodeJS.Timeout | undefined;

/**
 * Map of active debounce timers keyed by document URI string for single-document live validation.
 */
const validateTimers = new Map<string, NodeJS.Timeout>();

/**
 * Version tracking map (URI string -> document.version).
 * Prevents out-of-order asynchronous daemon responses from applying stale diagnostics to newer document states.
 */
const latestValidationVersions = new Map<string, number>();

// ─── Activation ───────────────────────────────────────────────────────────────

/**
 * Main activation function called by VS Code when matching activation events fire.
 * Initializes diagnostics, status bar items, analyzer daemons, language providers, and file watchers.
 *
 * @param context - Extension context provided by VS Code runtime.
 */
export async function activate(context: vscode.ExtensionContext): Promise<void> {
  // 1. Create and register the single shared Status Bar item
  statusBarItem = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Left);
  context.subscriptions.push(statusBarItem);

  // 2. Initialize diagnostic collections with dedicated namespaces
  outputChannel = vscode.window.createOutputChannel('GoTpl LSP');
  analyzerCollection = vscode.languages.createDiagnosticCollection('gotpl-analyzer');
  editorCollection = vscode.languages.createDiagnosticCollection('gotpl-editor');
  namedBlockCollection = vscode.languages.createDiagnosticCollection('gotpl-named-blocks');

  context.subscriptions.push(
    outputChannel,
    analyzerCollection,
    editorCollection,
    namedBlockCollection
  );
  outputChannel.appendLine('[GoTpl] Extension activated');

  // 3. Check if extension is explicitly disabled in workspace/user configuration
  if (!config.enabled()) {
    outputChannel.appendLine('[GoTpl] Extension disabled via gotpl.enabled setting.');
    statusBarItem.text = '$(circle-slash) GoTpl: Disabled';
    statusBarItem.tooltip = 'Go Template LSP is disabled. Set gotpl.enabled to true to enable.';
    statusBarItem.show();

    // Listen for configuration toggle so the extension can be re-enabled without reloading manually
    context.subscriptions.push(
      vscode.workspace.onDidChangeConfiguration((e) => {
        if (e.affectsConfiguration('gotpl.enabled')) {
          config.reload();
          if (config.enabled()) {
            vscode.commands.executeCommand('workbench.action.reloadWindow');
          }
        }
      })
    );
    return;
  }

  // 4. Resolve workspace root folder
  const workspaceRoot = vscode.workspace.workspaceFolders?.[0]?.uri.fsPath;
  if (!workspaceRoot) {
    outputChannel.appendLine('[GoTpl] No workspace folder found. Go Template LSP inactive.');
    return;
  }

  // 5. Ensure the analyzer binary exists or prompt user / download / build in dev mode
  const analyzerExecutablePath = await AnalyzerInstaller.ensureInstalled(context, outputChannel);
  if (!analyzerExecutablePath) {
    outputChannel.appendLine('[GoTpl] Analyzer not installed. Extension disabled.');
    return;
  }

  // 6. Instantiate core engine components
  analyzer = new GoAnalyzer(analyzerExecutablePath, outputChannel);
  graphBuilder = new KnowledgeGraphBuilder(workspaceRoot, outputChannel, statusBarItem);
  setKnowledgeGraphBuilder(graphBuilder);
  validator = new TemplateValidator(outputChannel, graphBuilder, analyzer);

  // ── Register User Commands ──────────────────────────────────────────────────

  context.subscriptions.push(
    // Command: Manually validate the active template editor
    vscode.commands.registerCommand('gotpl.validate', async () => {
      const doc = vscode.window.activeTextEditor?.document;
      if (doc && isTemplate(doc)) {
        await validateDocument(doc);
      }
    }),

    // Command: Force a full re-analysis of Go source files and templates
    vscode.commands.registerCommand('gotpl.rebuildIndex', async () => {
      await rebuildIndex(workspaceRoot, 'full');
    }),

    // Command: Render the interactive Knowledge Graph webview panel
    vscode.commands.registerCommand('gotpl.showKnowledgeGraph', () => {
      if (currentGraph) {
        KnowledgeGraphPanel.show(context, currentGraph);
      } else {
        vscode.window.showInformationMessage(
          'No template index available yet. Please wait for analysis or run "GoTpl: Rebuild Template Index".'
        );
      }
    }),

    // Command: Check proxy.golang.org for newer releases of gotpl-analyzer
    vscode.commands.registerCommand('gotpl.checkUpdates', async () => {
      const analyzerPath = await AnalyzerInstaller.getAnalyzerPath();
      if (!analyzerPath) {
        vscode.window.showWarningMessage('gotpl-analyzer is not installed.');
        return;
      }
      await AnalyzerInstaller.checkForUpdates(analyzerPath, outputChannel);
    }),

    // Command: Jump from current template to the Go handler(s) that render it
    vscode.commands.registerCommand('gotpl.goToRenderCall', async (uri?: vscode.Uri) => {
      const targetUri = uri ?? vscode.window.activeTextEditor?.document.uri;
      if (!targetUri || !graphBuilder) return;

      const fsPath = targetUri.fsPath;
      let ctx = graphBuilder.findContextForFile(fsPath);
      if (!ctx || ctx.renderCalls.length === 0) {
        ctx = await graphBuilder.findContextForFileAsPartialAsync(fsPath);
      }

      const calls = (ctx?.renderCalls ?? []).filter(
        (rc) => rc.file && rc.file !== 'template-call' && rc.file !== 'context-file'
      );

      if (calls.length === 0) {
        vscode.window.showInformationMessage(`No Go render calls found for "${path.basename(fsPath)}".`);
        return;
      }

      if (calls.length === 1) {
        await openRenderCallLocation(calls[0]);
        return;
      }

      // If rendered in multiple places, show a QuickPick selector
      const items = calls.map((rc) => ({
        label: `$(go-file) ${rc.file}:${rc.line}`,
        description: `Template: ${rc.template}`,
        detail: rc.vars?.length ? `Vars: ${rc.vars.map((v) => v.name).join(', ')}` : undefined,
        renderCall: rc,
      }));

      const selected = await vscode.window.showQuickPick(items, {
        placeHolder: `Select a render call for ${path.basename(fsPath)} (${calls.length} found)`,
      });

      if (selected) {
        await openRenderCallLocation(selected.renderCall);
      }
    }),
  );

  // ── Register LSP Providers ──────────────────────────────────────────────────

  context.subscriptions.push(
    vscode.languages.registerReferenceProvider(TEMPLATE_SELECTOR, {
      async provideReferences(
        document: vscode.TextDocument,
        position: vscode.Position,
        context: vscode.ReferenceContext
      ) {
        if (!validator) return null;
        return await validator.getReferences(document, position, context.includeDeclaration);
      },
    }),

    // Hover Provider: Resolves type signatures, docs, and struct fields for {{ .Var }}
    vscode.languages.registerHoverProvider(TEMPLATE_SELECTOR, {
      async provideHover(document: vscode.TextDocument, position: vscode.Position) {
        if (!validator || !graphBuilder) return null;

        // Try primary context resolution first
        let ctx = graphBuilder.findContextForFile(document.uri.fsPath);
        // Fallback: If this file is an unrendered partial, resolve context from parent call sites
        if (!ctx || ctx.vars.size === 0) {
          const partialCtx = await graphBuilder.findContextForFileAsPartialAsync(document.uri.fsPath);
          if (partialCtx) ctx = partialCtx;
        }
        if (!ctx) return null;

        return validator.getHoverInfo(document, position, ctx);
      },
    }),

    // Completion Item Provider: Triggers on '.', '$', and '"' (for template names)
    vscode.languages.registerCompletionItemProvider(
      TEMPLATE_SELECTOR,
      {
        async provideCompletionItems(document: vscode.TextDocument, position: vscode.Position) {
          if (!validator || !graphBuilder) return [];

          let ctx = graphBuilder.findContextForFile(document.uri.fsPath);
          if (!ctx || ctx.vars.size === 0) {
            const partialCtx = await graphBuilder.findContextForFileAsPartialAsync(document.uri.fsPath);
            if (partialCtx) ctx = partialCtx;
          }
          if (!ctx) return [];

          return await validator.getCompletionItems(document, position, ctx);
        },
      },
      '.', '$', '"'
    ),

    // Definition Provider (Templates): Jumps to Go struct fields, FuncMap funcs, or {{ define }} blocks
    vscode.languages.registerDefinitionProvider(TEMPLATE_SELECTOR, {
      async provideDefinition(document: vscode.TextDocument, position: vscode.Position) {
        if (!validator || !graphBuilder) return null;

        let ctx = graphBuilder.findContextForFile(document.uri.fsPath);
        if (!ctx || ctx.vars.size === 0) {
          const partialCtx = await graphBuilder.findContextForFileAsPartialAsync(document.uri.fsPath);
          if (partialCtx) ctx = partialCtx;
        }
        if (!ctx) return null;

        return await validator.getDefinitionLocation(document, position, ctx);
      },
    }),

    // Definition Provider (Go Code): Jumps from `c.Render("users/list.html", ...)` to the template file
    vscode.languages.registerDefinitionProvider(GO_SELECTOR, {
      provideDefinition(document: vscode.TextDocument, position: vscode.Position) {
        if (!validator) return null;
        return validator.getTemplateDefinitionFromGo(document, position);
      },
    })
  );

  // ── Document Event Subscriptions ────────────────────────────────────────────

  context.subscriptions.push(
    // Triggered when a document is opened in the editor
    vscode.workspace.onDidOpenTextDocument((doc) => {
      if (isTemplate(doc)) {
        latestValidationVersions.set(doc.uri.toString(), doc.version);

        // Synchronize in-memory template content with the daemon so fast validations don't read disk
        if (analyzer) {
          void analyzer.updateTemplate(workspaceRoot, doc.uri.fsPath, doc.getText()).catch((err) => {
            outputChannel.appendLine(`[GoTpl] Failed to sync open template ${doc.uri.fsPath}: ${err}`);
          });
        }
        void validateDocument(doc, doc.version);
      }
    }),

    // Triggered on every keystroke/edit inside a document
    vscode.workspace.onDidChangeTextDocument((e) => {
      const doc = e.document;
      if (isTemplate(doc)) {
        // Fast AST update for local named-block structure
        if (graphBuilder) {
          try {
            graphBuilder.updateTemplateFile(doc.uri.fsPath, doc.getText());
            applyNamedBlockDiagnostics();
          } catch (err) {
            outputChannel.appendLine(`[GoTpl] Incremental graph update failed for ${doc.uri.fsPath}: ${err}`);
          }
        }

        latestValidationVersions.set(doc.uri.toString(), doc.version);

        // Sync modified buffer to daemon
        if (analyzer) {
          void analyzer.updateTemplate(workspaceRoot, doc.uri.fsPath, doc.getText()).catch((err) => {
            outputChannel.appendLine(`[GoTpl] Failed to sync template buffer ${doc.uri.fsPath}: ${err}`);
          });
        }

        // Debounce live validation of active document and cross-dependent open templates
        scheduleValidateDocument(doc);
        scheduleValidateOpenTemplateDocuments(doc.uri.toString());
      }
    }),

    // Triggered when an editor tab is closed
    vscode.workspace.onDidCloseTextDocument((doc) => {
      if (!isTemplate(doc)) return;

      const key = doc.uri.toString();
      const timer = validateTimers.get(key);
      if (timer) {
        clearTimeout(timer);
        validateTimers.delete(key);
      }
      latestValidationVersions.delete(key);

      // Notify the daemon to evict this file's in-memory buffer
      if (analyzer) {
        void analyzer.clearTemplate(workspaceRoot, doc.uri.fsPath).catch((err) => {
          outputChannel.appendLine(`[GoTpl] Failed to clear template sync ${doc.uri.fsPath}: ${err}`);
        });
      }
    }),

    // Triggered when a file is saved to disk
    vscode.workspace.onDidSaveTextDocument((doc) => {
      // 1. If it's a template file, only run lightweight template revalidation
      if (isTemplate(doc)) {
        scheduleRebuild(workspaceRoot, 'templates-only');
        return;
      }

      // 2. Strict check for Go source or specifically configured context JSON files.
      // NOTE: We deliberately do NOT match generic `.json` files here (e.g. settings.json,
      // package.json) to prevent triggering expensive Go AST parsing on unrelated edits.
      const fileName = doc.fileName;
      const contextFilePath = config.contextFile()
        ? path.resolve(workspaceRoot, config.contextFile())
        : '';

      const isGoSource = fileName.endsWith('.go') || fileName.endsWith('go.mod') || fileName.endsWith('go.sum');
      const isConfiguredContextFile = Boolean(contextFilePath && path.resolve(fileName) === contextFilePath);

      if (isGoSource || isConfiguredContextFile) {
        scheduleRebuild(workspaceRoot, 'full');
      }
    })
  );

  // ── Configuration Change Listener ──────────────────────────────────────────

  context.subscriptions.push(
    vscode.workspace.onDidChangeConfiguration((e) => {
      // Only react if settings under the `gotpl` namespace changed
      if (!e.affectsConfiguration('gotpl')) return;

      config.reload();

      if (!config.enabled()) {
        outputChannel.appendLine('[GoTpl] Extension disabled via settings.');
        vscode.commands.executeCommand('workbench.action.reloadWindow');
        return;
      }

      // Filter for settings that actually alter Go AST extraction or template roots.
      // Changing UI settings like debounceMs, validate, or showCallSiteErrors should NOT rebuild.
      const affectsGoAnalysis =
        e.affectsConfiguration('gotpl.sourceDir') ||
        e.affectsConfiguration('gotpl.templateRoot') ||
        e.affectsConfiguration('gotpl.templateBaseDir') ||
        e.affectsConfiguration('gotpl.contextFile') ||
        e.affectsConfiguration('gotpl.goAnalyzerPath') ||
        e.affectsConfiguration('gotpl.renderFunctionNames') ||
        e.affectsConfiguration('gotpl.setFunctionNames') ||
        e.affectsConfiguration('gotpl.contextTypeNames');

      if (affectsGoAnalysis) {
        outputChannel.appendLine('[GoTpl] Analysis configuration changed. Scheduling full rebuild...');
        scheduleRebuild(workspaceRoot, 'full');
      }
    })
  );

  // 7. Initial asynchronous workspace analysis on startup
  void rebuildIndex(workspaceRoot, 'full');
  outputChannel.appendLine('[GoTpl] Ready');
}

// ─── Helper Functions ─────────────────────────────────────────────────────────

/**
 * Checks whether a text document is a Go template based on file scheme and extension.
 *
 * @param doc - TextDocument to evaluate.
 * @returns True if the document is a local HTML or template file.
 */
function isTemplate(doc: vscode.TextDocument): boolean {
  return (
    doc.uri.scheme === 'file' &&
    (doc.fileName.endsWith('.html') || doc.fileName.endsWith('.tmpl'))
  );
}

/**
 * Executes or queues a workspace analysis rebuild.
 *
 * Concurrency Safety:
 * If an analysis is already in progress (`isRebuilding === true`), subsequent requests are
 * merged into `pendingRebuildMode`. When the active build finishes, the queued build runs
 * automatically without running overlapping `go/packages.Load` invocations.
 *
 * @param workspaceRoot - Absolute path to workspace root.
 * @param mode - 'full' to re-parse Go AST + templates; 'templates-only' to reuse cached Go types.
 */
async function rebuildIndex(workspaceRoot: string, mode: 'full' | 'templates-only' = 'full'): Promise<void> {
  if (!analyzer || !graphBuilder) return;

  if (isRebuilding) {
    // If a full rebuild is requested while a template-only build runs, upgrade the queued mode
    if (mode === 'full' || pendingRebuildMode === 'full') {
      pendingRebuildMode = 'full';
    } else {
      pendingRebuildMode = 'templates-only';
    }
    return;
  }

  isRebuilding = true;
  const label = mode === 'templates-only' ? 'Re-validating templates...' : 'Analyzing Go sources...';
  statusBarItem.text = `$(sync~spin) GoTpl: ${label}`;
  statusBarItem.show();

  const sourceDir = config.sourceDir();
  const templateRoot = config.templateRoot();
  const templateBaseDir = config.templateBaseDir();

  try {
    const startTime = Date.now();
    const result = mode === 'templates-only'
      ? await analyzer.reanalyzeTemplates(workspaceRoot)
      : await analyzer.analyzeWorkspace(workspaceRoot);

    // Build the in-memory KnowledgeGraph from Go analysis results
    currentGraph = graphBuilder.build(result);

    const elapsed = Date.now() - startTime;
    outputChannel.appendLine(
      `[GoTpl] ${mode === 'templates-only' ? 'Template reanalysis' : 'Full analysis'} completed in ${elapsed}ms`
    );

    // Publish diagnostics across collections
    await applyAnalyzerDiagnostics(
      result.validationErrors ?? [],
      workspaceRoot,
      sourceDir,
      templateRoot,
      templateBaseDir
    );
    applyNamedBlockDiagnostics();
    await validateOpenTemplateDocuments();

    // Update status bar with indexed template count
    const count = currentGraph.templates.size;
    statusBarItem.text = `$(check) GoTpl: ${count} template${count === 1 ? '' : 's'} indexed`;
    statusBarItem.show();
    setTimeout(() => statusBarItem.hide(), 5000);
  } catch (err) {
    outputChannel.appendLine(`[GoTpl] Rebuild failed: ${err}`);
    statusBarItem.text = '$(error) GoTpl: Analysis failed';
    statusBarItem.show();
  } finally {
    isRebuilding = false;

    // Drain queued rebuild if another change arrived while rebuilding
    if (pendingRebuildMode) {
      const nextMode = pendingRebuildMode;
      pendingRebuildMode = null;
      void rebuildIndex(workspaceRoot, nextMode);
    }
  }
}

/**
 * Scans the KnowledgeGraph for duplicate `{{ define "name" }}` or `{{ block "name" }}`
 * declarations and publishes diagnostic errors to all files declaring the duplicated name.
 */
function applyNamedBlockDiagnostics(): void {
  if (!graphBuilder) return;
  namedBlockCollection.clear();

  const duplicateErrors: NamedBlockDuplicateError[] = graphBuilder.getAllDuplicateErrors();
  if (duplicateErrors.length === 0) return;

  const issuesByFile = new Map<string, vscode.Diagnostic[]>();

  for (const err of duplicateErrors) {
    for (const entry of err.entries) {
      // Build a human-readable list of other locations where this block name was defined
      const locs = err.entries
        .filter((e) => e.absolutePath !== entry.absolutePath || e.line !== entry.line)
        .map((e) => `${e.templatePath}:${e.line}`)
        .join(', ');

      const msg =
        `Duplicate named block "${err.name}". ` +
        `Also declared at: ${locs}. Only one declaration is allowed project-wide.`;

      const range = new vscode.Range(
        Math.max(0, entry.line - 1),
        Math.max(0, entry.col - 1),
        Math.max(0, entry.line - 1),
        Math.max(0, entry.col - 1) + entry.name.length + 2
      );

      const diag = new vscode.Diagnostic(range, msg, vscode.DiagnosticSeverity.Error);
      diag.source = 'GoTpl';
      diag.code = 'duplicate-named-block';

      const list = issuesByFile.get(entry.absolutePath) ?? [];
      list.push(diag);
      issuesByFile.set(entry.absolutePath, list);
    }
  }

  for (const [filePath, issues] of issuesByFile) {
    namedBlockCollection.set(vscode.Uri.file(filePath), issues);
  }
}

/**
 * Transforms Go analyzer validation errors into VS Code diagnostics and publishes them
 * to the `analyzerCollection`.
 *
 * Tricky Logic Handled:
 * 1. Partial Relocation: If an error occurs inside a shared partial template (`err.sourceTemplate`),
 *    the primary diagnostic is placed directly on the offending line inside the partial, while
 *    a `DiagnosticRelatedInformation` link is attached pointing back to the parent call-site.
 * 2. Origin Tracing: If an error is caused by a variable injected via `c.Render()`, a related
 *    information link points directly to the Go handler line.
 *
 * @param validationErrors - List of errors returned by `gotpl-analyzer`.
 * @param workspaceRoot - Root directory path of the workspace.
 * @param sourceDir - Go source root relative to workspace root.
 * @param templateRoot - Template folder root.
 * @param templateBaseDir - Optional override for template base directory.
 */
async function applyAnalyzerDiagnostics(
  validationErrors: GoValidationError[],
  workspaceRoot: string,
  sourceDir: string,
  templateRoot: string,
  templateBaseDir: string
): Promise<void> {
  analyzerCollection.clear();
  const issuesByFile = new Map<string, vscode.Diagnostic[]>();

  for (const err of validationErrors) {
    let diagnosticFilePath: string;
    let diagnosticLine: number;
    let diagnosticCol: number;
    let diagnosticEndCol: number;
    let relatedInfo: vscode.DiagnosticRelatedInformation[] | undefined;

    const baseDir = templateBaseDir
      ? path.join(workspaceRoot, templateBaseDir)
      : path.join(workspaceRoot, sourceDir);

    // Case 1: Error originated inside a partial / block invoked by another template
    if (err.sourceTemplate && err.sourceLine) {
      diagnosticFilePath = path.join(baseDir, templateRoot, err.sourceTemplate);
      diagnosticLine = Math.max(0, err.sourceLine - 1);
      diagnosticCol = Math.max(0, (err.sourceColumn ?? 1) - 1);
      diagnosticEndCol = diagnosticCol + (err.variable?.length || 1);

      const callSitePath = path.join(baseDir, templateRoot, err.template);
      relatedInfo = [
        new vscode.DiagnosticRelatedInformation(
          new vscode.Location(
            vscode.Uri.file(callSitePath),
            new vscode.Position(Math.max(0, err.line - 1), Math.max(0, err.column - 1))
          ),
          `Referenced from call-site in ${err.template}`
        ),
      ];

      if (err.goFile) {
        const goFileAbs = path.join(path.resolve(workspaceRoot, sourceDir), err.goFile);
        relatedInfo.push(
          new vscode.DiagnosticRelatedInformation(
            new vscode.Location(
              vscode.Uri.file(goFileAbs),
              new vscode.Position(Math.max(0, (err.goLine ?? 1) - 1), 0)
            ),
            'Variable passed from Go render call'
          )
        );
      }
    } else {
      // Case 2: Direct error in top-level template
      diagnosticFilePath = path.join(baseDir, templateRoot, err.template);
      diagnosticLine = Math.max(0, err.line - 1);
      diagnosticCol = Math.max(0, err.column - 1);
      diagnosticEndCol = diagnosticCol + (err.variable?.length || 1);

      if (err.goFile) {
        const goFileAbs = path.join(path.resolve(workspaceRoot, sourceDir), err.goFile);
        relatedInfo = [
          new vscode.DiagnosticRelatedInformation(
            new vscode.Location(
              vscode.Uri.file(goFileAbs),
              new vscode.Position(Math.max(0, (err.goLine ?? 1) - 1), 0)
            ),
            'Variable passed from Go render call'
          ),
        ];
      }
    }

    const range = new vscode.Range(diagnosticLine, diagnosticCol, diagnosticLine, diagnosticEndCol);
    const diag = new vscode.Diagnostic(
      range,
      err.message,
      err.severity === 'warning' ? vscode.DiagnosticSeverity.Warning : vscode.DiagnosticSeverity.Error
    );
    diag.source = 'GoTpl';
    if (relatedInfo) {
      diag.relatedInformation = relatedInfo;
    }

    const list = issuesByFile.get(diagnosticFilePath) ?? [];
    list.push(diag);
    issuesByFile.set(diagnosticFilePath, list);
  }

  for (const [filePath, issues] of issuesByFile) {
    analyzerCollection.set(vscode.Uri.file(filePath), issues);
  }
}

/**
 * Validates a single open template document against the Go analyzer daemon.
 *
 * Out-of-Order Safety:
 * Validates using `requestedVersion`. If newer edits occurred while the daemon was calculating,
 * the response is discarded to prevent stale squiggles from overwriting newer diagnostics.
 *
 * @param doc - Document to validate.
 * @param requestedVersion - Document version at the moment validation was triggered.
 */
async function validateDocument(doc: vscode.TextDocument, requestedVersion: number = doc.version): Promise<void> {
  if (!analyzer) return;

  const docKey = doc.uri.toString();
  const latestVersion = latestValidationVersions.get(docKey);

  // Discard if document version has progressed past requestedVersion
  if (latestVersion !== undefined && latestVersion > requestedVersion) {
    return;
  }

  const workspaceRoot = vscode.workspace.workspaceFolders?.[0]?.uri.fsPath;
  if (!workspaceRoot) {
    editorCollection.delete(doc.uri);
    return;
  }

  try {
    const result = await analyzer.validateTemplate(workspaceRoot, doc.uri.fsPath, doc.getText());

    // Check version again after async round-trip
    if (latestValidationVersions.get(docKey) !== requestedVersion) {
      return;
    }

    const errors = result.validationErrors ?? [];
    if (!result.hasContext) {
      // If template is unrendered and has no detectable context, do not report false positives
      editorCollection.delete(doc.uri);
      return;
    }

    // Set live diagnostics on editor collection and clear any stale batch analyzer diagnostics for this file
    editorCollection.set(doc.uri, diagnosticsFromValidationErrors(errors));
    analyzerCollection.delete(doc.uri);
  } catch (err) {
    outputChannel.appendLine(`[GoTpl] Live validation failed for ${doc.uri.fsPath}: ${err}`);
  }
}

/**
 * Debounces a workspace index rebuild using `gotpl.debounceMs`.
 *
 * @param workspaceRoot - Workspace root path.
 * @param mode - Rebuild mode ('full' | 'templates-only').
 */
function scheduleRebuild(workspaceRoot: string, mode: 'full' | 'templates-only' = 'full'): void {
  const debounceMs = config.debounceMs();
  if (rebuildTimer) clearTimeout(rebuildTimer);
  rebuildTimer = setTimeout(() => {
    void rebuildIndex(workspaceRoot, mode);
  }, debounceMs);
}

/**
 * Debounces live validation for a specific document on keystrokes.
 *
 * @param doc - Modified TextDocument.
 */
function scheduleValidateDocument(doc: vscode.TextDocument): void {
  const debounceMs = config.debounceMs();
  const docKey = doc.uri.toString();
  latestValidationVersions.set(docKey, doc.version);

  const existingTimer = validateTimers.get(docKey);
  if (existingTimer) clearTimeout(existingTimer);

  const scheduledVersion = doc.version;
  const timer = setTimeout(async () => {
    validateTimers.delete(docKey);
    const currentDoc = vscode.workspace.textDocuments.find((openDoc) => openDoc.uri.toString() === docKey);
    if (!currentDoc || !isTemplate(currentDoc)) return;

    await validateDocument(currentDoc, scheduledVersion);
  }, debounceMs);

  validateTimers.set(docKey, timer);
}

/**
 * Schedules validation across other open template tabs (useful when editing shared partials/blocks).
 *
 * @param excludeDocKey - Optional URI string of document to skip (typically the one currently being typed in).
 */
function scheduleValidateOpenTemplateDocuments(excludeDocKey?: string): void {
  const debounceMs = config.debounceMs();

  if (validateOpenTemplatesTimer) clearTimeout(validateOpenTemplatesTimer);
  validateOpenTemplatesTimer = setTimeout(async () => {
    validateOpenTemplatesTimer = undefined;
    await validateOpenTemplateDocuments(excludeDocKey);
  }, debounceMs);
}

/**
 * Iterates all open TextDocuments and validates any open template files.
 *
 * @param excludeDocKey - Document URI to skip.
 */
async function validateOpenTemplateDocuments(excludeDocKey?: string): Promise<void> {
  const openDocs = vscode.workspace.textDocuments.filter(isTemplate);
  for (const doc of openDocs) {
    if (excludeDocKey && doc.uri.toString() === excludeDocKey) continue;
    await validateDocument(doc);
  }
}

/**
 * Maps an array of backend `GoValidationError` records into VS Code `Diagnostic` objects.
 *
 * @param errors - Raw error array from Go analyzer.
 * @returns Array of VS Code diagnostics.
 */
function diagnosticsFromValidationErrors(errors: GoValidationError[]): vscode.Diagnostic[] {
  if (!errors) return [];

  return errors.map((err) => {
    // Offset by -1 because Go AST lines/columns are 1-based while VS Code is 0-based
    const line = Math.max(0, (err.sourceLine ?? err.line) - 1);
    const col = Math.max(0, (err.sourceColumn ?? err.column) - 1);
    const range = new vscode.Range(line, col, line, col + (err.variable?.length || 1));

    const diagnostic = new vscode.Diagnostic(
      range,
      err.message,
      err.severity === 'warning' ? vscode.DiagnosticSeverity.Warning : vscode.DiagnosticSeverity.Error
    );
    diagnostic.source = 'GoTpl';
    return diagnostic;
  });
}

/**
 * Opens the Go file where a template is rendered and positions the cursor on the render call.
 */
async function openRenderCallLocation(rc: RenderCall): Promise<void> {
  if (!graphBuilder) return;

  const absPath = graphBuilder.resolveGoFilePath(rc.file);
  if (!absPath || !fs.existsSync(absPath)) {
    vscode.window.showErrorMessage(`Could not find Go file: ${rc.file}`);
    return;
  }

  const doc = await vscode.workspace.openTextDocument(absPath);
  const editor = await vscode.window.showTextDocument(doc);
  const line = Math.max(0, rc.line - 1);
  const col = Math.max(0, rc.templateNameStartCol ? rc.templateNameStartCol - 1 : 0);
  const pos = new vscode.Position(line, col);

  editor.selection = new vscode.Selection(pos, pos);
  editor.revealRange(new vscode.Range(pos, pos), vscode.TextEditorRevealType.InCenter);
}


// ─── Deactivation ─────────────────────────────────────────────────────────────

/**
 * Teardown hook invoked when the extension is deactivated or when VS Code reloads.
 * Cancels active timers, disposes diagnostics collections, and gracefully stops the daemon.
 */
export function deactivate(): void {
  // Clear all pending timers
  if (rebuildTimer) clearTimeout(rebuildTimer);
  if (validateOpenTemplatesTimer) clearTimeout(validateOpenTemplatesTimer);
  for (const timer of validateTimers.values()) {
    clearTimeout(timer);
  }
  validateTimers.clear();
  latestValidationVersions.clear();

  // Terminate backend process and release diagnostic collections
  analyzer?.dispose();
  analyzerCollection?.dispose();
  editorCollection?.dispose();
  namedBlockCollection?.dispose();
  outputChannel?.dispose();
  statusBarItem?.dispose();
}
