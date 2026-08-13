import * as vscode from 'vscode';
import * as path from 'path';
import * as fs from 'fs';
import { GoAnalyzer } from './analyzer';
import { KnowledgeGraphBuilder, setKnowledgeGraphBuilder } from './knowledgeGraph';
import { TemplateValidator } from './validator';
import { KnowledgeGraphPanel } from './graphPanel';
import { KnowledgeGraph, GoValidationError, NamedBlockDuplicateError } from './types';
import { config } from './config';
import { AnalyzerInstaller } from './installer';

const TEMPLATE_SELECTOR: vscode.DocumentSelector = [
  { language: 'html', scheme: 'file' },
  { language: 'go-template', scheme: 'file' },
  { pattern: '**/*.tmpl' },
  { pattern: '**/*.html' },
];

// Three separate collections so they never interfere with each other:
// - analyzerCollection:   diagnostics from the Go binary (persists across template edits)
// - editorCollection:     live diagnostics from the Go daemon for open documents
// - namedBlockCollection: duplicate named-block errors (cross-file, rebuilt with index)
let analyzerCollection: vscode.DiagnosticCollection;
let editorCollection: vscode.DiagnosticCollection;
let namedBlockCollection: vscode.DiagnosticCollection;
let outputChannel: vscode.OutputChannel;
let graphBuilder: KnowledgeGraphBuilder | undefined;
let validator: TemplateValidator | undefined;
let currentGraph: KnowledgeGraph | undefined;
let analyzer: GoAnalyzer | undefined;

/**
 * Single status bar item shared across the extension and KnowledgeGraphBuilder.
 * extension.ts owns its lifetime (created here, disposed in deactivate).
 * KnowledgeGraphBuilder receives it by reference and only mutates .text / .show() / .hide().
 */
let statusBarItem: vscode.StatusBarItem;

let rebuildTimer: NodeJS.Timeout | undefined;
let validateOpenTemplatesTimer: NodeJS.Timeout | undefined;
const validateTimers = new Map<string, NodeJS.Timeout>();
const latestValidationVersions = new Map<string, number>();

export async function activate(context: vscode.ExtensionContext) {
  // Create the single shared status bar item.
  statusBarItem = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Left);
  context.subscriptions.push(statusBarItem);

  outputChannel = vscode.window.createOutputChannel('GoTpl LSP');
  analyzerCollection = vscode.languages.createDiagnosticCollection('gotpl-analyzer');
  editorCollection = vscode.languages.createDiagnosticCollection('gotpl-editor');
  namedBlockCollection = vscode.languages.createDiagnosticCollection('gotpl-named-blocks');

  context.subscriptions.push(outputChannel, analyzerCollection, editorCollection, namedBlockCollection);
  outputChannel.appendLine('[GoTpl] Extension activated');

  // Check if extension is disabled via settings
  if (!config.enabled()) {
    outputChannel.appendLine('[GoTpl] Extension disabled via gotpl.enabled setting.');
    statusBarItem.text = '$(circle-slash) GoTpl: Disabled';
    statusBarItem.tooltip = 'Go Template LSP is disabled. Set gotpl.enabled to true to enable.';
    statusBarItem.show();
    // Still listen for config changes so user can re-enable at runtime
    context.subscriptions.push(
      vscode.workspace.onDidChangeConfiguration((e) => {
        if (e.affectsConfiguration('gotpl')) {
          config.reload();
          if (config.enabled()) {
            vscode.commands.executeCommand('workbench.action.reloadWindow');
          }
        }
      })
    );
    return;
  }

  const workspaceRoot = vscode.workspace.workspaceFolders?.[0]?.uri.fsPath;
  if (!workspaceRoot) {
    outputChannel.appendLine('[GoTpl] No workspace folder found');
    return;
  }

  const analyzerExecutablePath = await AnalyzerInstaller.ensureInstalled(context, outputChannel);
  if (!analyzerExecutablePath) {
    outputChannel.appendLine('[GoTpl] Analyzer not installed. Extension disabled.');
    return;
  }
  analyzer = new GoAnalyzer(analyzerExecutablePath, outputChannel);

  graphBuilder = new KnowledgeGraphBuilder(workspaceRoot, outputChannel, statusBarItem);
  setKnowledgeGraphBuilder(graphBuilder);
  validator = new TemplateValidator(outputChannel, graphBuilder, analyzer);

  // ── Commands ───────────────────────────────────────────────────────────────

  context.subscriptions.push(
    vscode.commands.registerCommand('gotpl.validate', async () => {
      const doc = vscode.window.activeTextEditor?.document;
      if (doc && isTemplate(doc)) {
        await validateDocument(doc);
      }
    }),

    vscode.commands.registerCommand('gotpl.rebuildIndex', async () => {
      await rebuildIndex(workspaceRoot, 'full');
    }),

    vscode.commands.registerCommand('gotpl.showKnowledgeGraph', () => {
      if (currentGraph) {
        KnowledgeGraphPanel.show(context, currentGraph);
      } else {
        vscode.window.showInformationMessage(
          'No template index yet. Run "GoTpl: Rebuild Template Index" first.'
        );
      }
    }),

    vscode.commands.registerCommand('gotpl.checkUpdates', async () => {
      const analyzerPath = await AnalyzerInstaller.getAnalyzerPath();
      if (!analyzerPath) {
        vscode.window.showWarningMessage('gotpl-analyzer is not installed.');
        return;
      }
      await AnalyzerInstaller.checkForUpdates(analyzerPath, outputChannel);
    })
  );

  // ── Language features ──────────────────────────────────────────────────────

  context.subscriptions.push(
    vscode.languages.registerHoverProvider(TEMPLATE_SELECTOR, {
      async provideHover(document, position, token) {
        if (!validator || !graphBuilder) return;

        let ctx = graphBuilder.findContextForFile(document.uri.fsPath);
        if (!ctx || ctx.vars.size === 0) {
          const partialCtx = await graphBuilder.findContextForFileAsPartialAsync(document.uri.fsPath);
          if (partialCtx) ctx = partialCtx;
        }
        if (!ctx) return;

        return validator.getHoverInfo(document, position, ctx);
      },
    })
  );

  context.subscriptions.push(
    vscode.languages.registerCompletionItemProvider(
      TEMPLATE_SELECTOR,
      {
        async provideCompletionItems(document, position) {
          if (!validator || !graphBuilder) return;
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
    )
  );

  context.subscriptions.push(
    vscode.languages.registerDefinitionProvider(TEMPLATE_SELECTOR, {
      async provideDefinition(document, position) {
        if (!validator || !graphBuilder) return;
        let ctx = graphBuilder.findContextForFile(document.uri.fsPath);
        if (!ctx || ctx.vars.size === 0) {
          const partialCtx = await graphBuilder.findContextForFileAsPartialAsync(document.uri.fsPath);
          if (partialCtx) ctx = partialCtx;
        }
        if (!ctx) return;
        return await validator.getDefinitionLocation(document, position, ctx);
      },
    })
  );

  const GO_SELECTOR: vscode.DocumentSelector = [{ language: 'go', scheme: 'file' }];
  context.subscriptions.push(
    vscode.languages.registerDefinitionProvider(GO_SELECTOR, {
      provideDefinition(document, position) {
        if (!validator) return;
        return validator.getTemplateDefinitionFromGo(document, position);
      },
    })
  );

  context.subscriptions.push(
    vscode.workspace.onDidOpenTextDocument((doc) => {
      if (isTemplate(doc)) {
        latestValidationVersions.set(doc.uri.toString(), doc.version);
        if (analyzer) {
          void analyzer.updateTemplate(workspaceRoot, doc.uri.fsPath, doc.getText()).catch((err) => {
            outputChannel.appendLine(`[GoTpl] Failed to sync open template ${doc.uri.fsPath}: ${err}`);
          });
        }
        validateDocument(doc, doc.version);
      }
    }),

    vscode.workspace.onDidChangeTextDocument((e) => {
      const doc = e.document;
      if (isTemplate(doc)) {
        if (graphBuilder) {
          try {
            graphBuilder.updateTemplateFile(doc.uri.fsPath, doc.getText());
            applyNamedBlockDiagnostics();
          } catch (err) {
            outputChannel.appendLine(`[GoTpl] Incremental graph update failed for ${doc.uri.fsPath}: ${err}`);
          }
        }
        latestValidationVersions.set(doc.uri.toString(), doc.version);
        if (analyzer) {
          void analyzer.updateTemplate(workspaceRoot, doc.uri.fsPath, doc.getText()).catch((err) => {
            outputChannel.appendLine(`[GoTpl] Failed to sync template ${doc.uri.fsPath}: ${err}`);
          });
        }
        scheduleValidateDocument(doc);
        scheduleValidateOpenTemplateDocuments(doc.uri.toString());
      }
    }),

    vscode.workspace.onDidCloseTextDocument((doc) => {
      if (!isTemplate(doc)) return;

      const key = doc.uri.toString();
      const timer = validateTimers.get(key);
      if (timer) {
        clearTimeout(timer);
        validateTimers.delete(key);
      }
      latestValidationVersions.delete(key);
      if (analyzer) {
        void analyzer.clearTemplate(workspaceRoot, doc.uri.fsPath).catch((err) => {
          outputChannel.appendLine(`[GoTpl] Failed to clear template sync ${doc.uri.fsPath}: ${err}`);
        });
      }
    }),

    vscode.workspace.onDidSaveTextDocument((doc) => {
      if (isTemplate(doc)) {
        // Template-only change: use fast incremental revalidation
        scheduleRebuild(workspaceRoot, 'templates-only');
        return;
      }

      if (doc.fileName.endsWith('.go') || doc.fileName.endsWith('go.mod') || doc.fileName.endsWith('.json')) {
        // Go source changed: full rebuild required
        scheduleRebuild(workspaceRoot, 'full');
      }
    })
  );

  context.subscriptions.push(
    vscode.workspace.onDidChangeConfiguration((e) => {
      if (e.affectsConfiguration('gotpl')) {
        config.reload();
        if (!config.enabled()) {
          outputChannel.appendLine('[GoTpl] Extension disabled via settings. Reload to take effect.');
          vscode.commands.executeCommand('workbench.action.reloadWindow');
          return;
        }
        outputChannel.appendLine('[GoTpl] Configuration changed, rebuilding index...');
        scheduleRebuild(workspaceRoot, 'full');
      }
    })
  );

  void rebuildIndex(workspaceRoot, 'full');
  outputChannel.appendLine('[GoTpl] Ready');
}

function isTemplate(doc: vscode.TextDocument): boolean {
  return (
    doc.uri.scheme === 'file' &&
    (doc.fileName.endsWith('.html') || doc.fileName.endsWith('.tmpl'))
  );
}

async function rebuildIndex(workspaceRoot: string, mode: 'full' | 'templates-only' = 'full') {
  if (!analyzer || !graphBuilder) return;

  const label = mode === 'templates-only' ? 'Re-validating templates...' : 'Analyzing Go sources...';
  statusBarItem.text = `$(sync~spin) GoTpl: ${label}`;
  statusBarItem.show();

  const sourceDir: string = config.sourceDir();
  const templateRoot: string = config.templateRoot();
  const templateBaseDir: string = config.templateBaseDir();

  try {
    const startTime = Date.now();
    const result = mode === 'templates-only'
      ? await analyzer.reanalyzeTemplates(workspaceRoot)
      : await analyzer.analyzeWorkspace(workspaceRoot);
    currentGraph = graphBuilder.build(result);

    const elapsed = Date.now() - startTime;
    outputChannel.appendLine(`[GoTpl] ${mode === 'templates-only' ? 'Template reanalysis' : 'Full analysis'} completed in ${elapsed}ms`);

    await applyAnalyzerDiagnostics(result.validationErrors ?? [], workspaceRoot, sourceDir, templateRoot, templateBaseDir);
    applyNamedBlockDiagnostics();
    await validateOpenTemplateDocuments();

    const count = currentGraph.templates.size;
    statusBarItem.text = `$(check) GoTpl: ${count} template${count === 1 ? '' : 's'} indexed`;
    statusBarItem.show();
    setTimeout(() => statusBarItem.hide(), 5000);
  } catch (err) {
    outputChannel.appendLine(`[GoTpl] Rebuild failed: ${err}`);
    statusBarItem.text = '$(error) GoTpl: Analysis failed';
    statusBarItem.show();
  }
}

function applyNamedBlockDiagnostics() {
  if (!graphBuilder) return;
  namedBlockCollection.clear();

  const duplicateErrors: NamedBlockDuplicateError[] = graphBuilder.getAllDuplicateErrors();
  if (duplicateErrors.length === 0) return;

  const issuesByFile = new Map<string, vscode.Diagnostic[]>();

  for (const err of duplicateErrors) {
    for (const entry of err.entries) {
      const locs = err.entries
        .filter(e => e.absolutePath !== entry.absolutePath || e.line !== entry.line)
        .map(e => `${e.templatePath}:${e.line}`)
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

async function applyAnalyzerDiagnostics(
  validationErrors: GoValidationError[],
  workspaceRoot: string,
  sourceDir: string,
  templateRoot: string,
  templateBaseDir: string
) {
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

    // Handles relocated error originating inside a partial or block template
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

async function validateDocument(doc: vscode.TextDocument, requestedVersion = doc.version) {
  if (!analyzer) return;

  const docKey = doc.uri.toString();
  const latestVersion = latestValidationVersions.get(docKey);
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
    if (latestValidationVersions.get(docKey) !== requestedVersion) {
      return;
    }

    const errors = result.validationErrors ?? [];
    if (!result.hasContext) {
      editorCollection.delete(doc.uri);
      return;
    }
    editorCollection.set(doc.uri, diagnosticsFromValidationErrors(errors));
    analyzerCollection.delete(doc.uri);
  } catch (err) {
    outputChannel.appendLine(`[GoTpl] Live validation failed for ${doc.uri.fsPath}: ${err}`);
  }
}

function scheduleRebuild(workspaceRoot: string, mode: 'full' | 'templates-only' = 'full') {
  const debounceMs = config.debounceMs();
  if (rebuildTimer) clearTimeout(rebuildTimer);
  rebuildTimer = setTimeout(() => rebuildIndex(workspaceRoot, mode), debounceMs);
}

function scheduleValidateDocument(doc: vscode.TextDocument) {
  const debounceMs = config.debounceMs();
  const docKey = doc.uri.toString();
  latestValidationVersions.set(docKey, doc.version);

  const existingTimer = validateTimers.get(docKey);
  if (existingTimer) clearTimeout(existingTimer);

  const scheduledVersion = doc.version;
  const timer = setTimeout(async () => {
    validateTimers.delete(docKey);
    const currentDoc = vscode.workspace.textDocuments.find(openDoc => openDoc.uri.toString() === docKey);
    if (!currentDoc || !isTemplate(currentDoc)) return;

    await validateDocument(currentDoc, scheduledVersion);
  }, debounceMs);

  validateTimers.set(docKey, timer);
}

function scheduleValidateOpenTemplateDocuments(excludeDocKey?: string) {
  const debounceMs = config.debounceMs();

  if (validateOpenTemplatesTimer) clearTimeout(validateOpenTemplatesTimer);
  validateOpenTemplatesTimer = setTimeout(async () => {
    validateOpenTemplatesTimer = undefined;
    await validateOpenTemplateDocuments(excludeDocKey);
  }, debounceMs);
}

async function validateOpenTemplateDocuments(excludeDocKey?: string) {
  const openDocs = vscode.workspace.textDocuments.filter(isTemplate);
  for (const doc of openDocs) {
    if (excludeDocKey && doc.uri.toString() === excludeDocKey) continue;
    await validateDocument(doc);
  }
}

function diagnosticsFromValidationErrors(errors: GoValidationError[]): vscode.Diagnostic[] {
  if (!errors) return [];

  return errors.map(err => {
    // Correctly locate error on sourceTemplate line if relocated from a partial
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

export function deactivate() {
  if (rebuildTimer) clearTimeout(rebuildTimer);
  if (validateOpenTemplatesTimer) clearTimeout(validateOpenTemplatesTimer);
  for (const timer of validateTimers.values()) {
    clearTimeout(timer);
  }
  validateTimers.clear();
  latestValidationVersions.clear();
  analyzer?.dispose();
  analyzerCollection?.dispose();
  editorCollection?.dispose();
  namedBlockCollection?.dispose();
  outputChannel?.dispose();
  statusBarItem?.dispose();
}
