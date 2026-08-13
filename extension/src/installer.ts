import * as vscode from 'vscode';
import * as cp from 'child_process';
import * as path from 'path';
import * as fs from 'fs';
import * as https from 'https';
import * as util from 'util';

const exec = util.promisify(cp.exec);
const MODULE_PATH = 'github.com/abiiranathan/go-template-lsp/gotpl-analyzer';
const BINARY_NAME = process.platform === 'win32' ? 'gotpl-analyzer.exe' : 'gotpl-analyzer';
const PROXY_URL = `https://proxy.golang.org/${MODULE_PATH.toLowerCase()}/@latest`;

export class AnalyzerInstaller {

    /**
     * Gets the installed version by executing `gotpl-analyzer -version`.
     */
    static async getInstalledVersion(analyzerPath: string): Promise<string | null> {
        try {
            const { stdout } = await exec(`"${analyzerPath}" -version`);
            const version = stdout.trim();
            return version ? version : null;
        } catch {
            return null;
        }
    }

    /**
     * Fetches the latest published version from the official Go proxy:
     * https://proxy.golang.org/github.com/abiiranathan/go-template-lsp/gotpl-analyzer/@latest
     */
    static async getLatestRemoteVersion(): Promise<string | null> {
        return new Promise((resolve) => {
            const req = https.get(PROXY_URL, { timeout: 3000 }, (res) => {
                if (res.statusCode !== 200) {
                    resolve(null);
                    return;
                }
                let rawData = '';
                res.on('data', (chunk) => (rawData += chunk));
                res.on('end', () => {
                    try {
                        const parsed = JSON.parse(rawData);
                        resolve(parsed.Version || null); // e.g. "v0.2.1"
                    } catch {
                        resolve(null);
                    }
                });
            });

            req.on('error', () => resolve(null));
            req.on('timeout', () => {
                req.destroy();
                resolve(null);
            });
        });
    }

    /**
     * Checks if an update is available and prompts the user.
     */
    static async checkForUpdates(analyzerPath: string, outputChannel: vscode.OutputChannel): Promise<void> {
        try {
            const [installed, latest] = await Promise.all([
                this.getInstalledVersion(analyzerPath),
                this.getLatestRemoteVersion(),
            ]);

            outputChannel.appendLine(`[Installer] Fetched versions: installed=${installed}, latest=${latest}`);

            if (!installed || !latest) {
                outputChannel.appendLine(`[Installer] Skipping update check — missing version info.`);
                return;
            }

            outputChannel.appendLine(`[Installer] Analyzer version: installed=${installed}, latest=${latest}`);

            if (this.isNewerVersion(installed, latest)) {
                const updateItem = 'Update Now';
                const selection = await vscode.window.showInformationMessage(
                    `A new version of gotpl-analyzer is available (${installed} → ${latest}).`,
                    updateItem
                );

                if (selection === updateItem) {
                    await this.installAnalyzer(outputChannel);
                }
            } else {
                await vscode.window.showInformationMessage(`gotpl-analyzer is already the newest version`);
            }
        } catch (err) {
            outputChannel.appendLine(`[Installer] Update check failed: ${err}`);
        }
    }

    /**
     * Installs or updates the analyzer using `go install ...@latest`.
     */
    static async installAnalyzer(outputChannel: vscode.OutputChannel): Promise<string | null> {
        return await vscode.window.withProgress({
            location: vscode.ProgressLocation.Notification,
            title: 'Updating gotpl-analyzer to latest...',
            cancellable: false,
        }, async () => {
            try {
                outputChannel.appendLine(`[Installer] Running: go install ${MODULE_PATH}@latest`);
                await exec(`go install ${MODULE_PATH}@latest`);

                const analyzerPath = await this.getAnalyzerPath();
                if (!analyzerPath) throw new Error('Binary not found in GOPATH/bin after install.');

                vscode.window.showInformationMessage('Go Template LSP analyzer updated successfully!');
                return analyzerPath;
            } catch (err) {
                outputChannel.appendLine(`[Installer] Update failed: ${err}`);
                vscode.window.showErrorMessage(`Failed to update gotpl-analyzer: ${err}`);
                return null;
            }
        });
    }

    /**
     * Simple SemVer comparison (returns true if remote > current).
     */
    private static isNewerVersion(current: string, remote: string): boolean {
        const parse = (v: string) => v.replace(/^v/, '').split('-')[0].split('.').map(n => parseInt(n, 10) || 0);
        const [cMajor, cMinor, cPatch] = parse(current);
        const [rMajor, rMinor, rPatch] = parse(remote);

        if (rMajor > cMajor) return true;
        if (rMajor === cMajor && rMinor > cMinor) return true;
        if (rMajor === cMajor && rMinor === cMinor && rPatch > cPatch) return true;
        return false;
    }

    static async ensureInstalled(context: vscode.ExtensionContext, outputChannel: vscode.OutputChannel): Promise<string | null> {
        const configPath = vscode.workspace.getConfiguration('gotpl').get<string>('goAnalyzerPath');
        if (configPath && fs.existsSync(configPath)) {
            return configPath;
        }

        if (context.extensionMode === vscode.ExtensionMode.Development) {
            const localBin = await this.buildLocalAnalyzer(context, outputChannel);
            if (localBin) return localBin;
        }

        let analyzerPath = await this.getAnalyzerPath();
        if (analyzerPath) {
            // Check for updates asynchronously in the background
            void this.checkForUpdates(analyzerPath, outputChannel);
            return analyzerPath;
        }

        const installItem = 'Install';
        const selection = await vscode.window.showInformationMessage(
            `The Go Template LSP requires the '${BINARY_NAME}' tool. Would you like to install it now?`,
            installItem
        );

        if (selection !== installItem) {
            vscode.window.showWarningMessage('Go Template LSP features will be disabled until the analyzer is installed.');
            return null;
        }

        return await this.installAnalyzer(outputChannel);
    }

    static async getGoBinPath(): Promise<string | null> {
        try {
            const { stdout } = await exec('go env GOPATH');
            const firstGoPath = stdout.trim().split(path.delimiter)[0];
            return path.join(firstGoPath, 'bin');
        } catch {
            return null;
        }
    }

    /**
     * Builds the analyzer locally when running in Extension Development Mode.
     */
    static async buildLocalAnalyzer(context: vscode.ExtensionContext, outputChannel: vscode.OutputChannel): Promise<string | null> {
        const analyzerSourceDir = path.join(context.extensionPath, '..', 'gotpl-analyzer');
        const outputBinary = path.join(analyzerSourceDir, BINARY_NAME);

        if (!fs.existsSync(analyzerSourceDir)) {
            return null;
        }

        outputChannel.appendLine('[Installer] Development mode detected. Building local analyzer...');
        try {
            await exec(`go build -o ${BINARY_NAME} .`, { cwd: analyzerSourceDir });
            outputChannel.appendLine('[Installer] Local build successful.');
            return outputBinary;
        } catch (err) {
            outputChannel.appendLine(`[Installer] Local build failed: ${err}`);
            return null;
        }
    }

    static async getAnalyzerPath(): Promise<string | null> {
        const goBin = await this.getGoBinPath();
        if (goBin) {
            const binPath = path.join(goBin, BINARY_NAME);
            if (fs.existsSync(binPath)) return binPath;
        }

        try {
            const command = process.platform === 'win32' ? 'where' : 'which';
            const { stdout } = await exec(`${command} ${BINARY_NAME}`);
            if (stdout.trim()) return stdout.trim().split('\n')[0];
        } catch { }

        return null;
    }
}
