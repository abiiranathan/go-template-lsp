import * as vscode from 'vscode';

// Centralized configuration management for the extension
// Namespace: gotpl
const extensionNamespace = 'gotpl';

class Config {
    private config: vscode.WorkspaceConfiguration;
    constructor() {
        this.config = vscode.workspace.getConfiguration(extensionNamespace);
    }

    private get<T>(name: string): T | undefined {
        return this.config.get<T>(name);
    }

    /** Whether the extension is enabled. When false, no analysis or LSP features activate. */
    enabled(): boolean {
        return this.get<boolean>('enabled') ?? true;
    }

    /** Path to the gotpl-analyzer binary. Empty string uses the bundled binary. */
    analyzerPath(): string {
        return this.get<string>('goAnalyzerPath') ?? '';
    }

    /** Go source directory relative to the workspace root. */
    sourceDir(): string {
        return this.get<string>('sourceDir') ?? '.';
    }

    /** Root directory for templates, relative to sourceDir. */
    templateRoot(): string {
        return this.get<string>('templateRoot') ?? '';
    }

    /** Base directory for templates, relative to the workspace root. */
    templateBaseDir(): string {
        return this.get<string>('templateBaseDir') ?? '';
    }

    /** Path to a JSON file with additional template context variables. */
    contextFile(): string {
        return this.get<string>('contextFile') ?? '';
    }

    /** Reload the configuration (e.g. after settings change). */
    reload(): void {
        this.config = vscode.workspace.getConfiguration(extensionNamespace);
    }

    /** Debounce delay (ms) before re-validating on file change. */
    debounceMs(): number {
        return this.get<number>('debounceMs') ?? 800;
    }

    /** Whether to enable gzip compression from the analyzer. */
    compress(): boolean {
        return this.get<boolean>('compress') ?? false;
    }

    /** Whether template validation is enabled. */
    validate(): boolean {
        return this.get<boolean>('validate') ?? true;
    }
}

export const config = new Config();
