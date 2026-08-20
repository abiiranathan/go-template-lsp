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
            const options: https.RequestOptions = {
                headers: {
                    'User-Agent': 'vscode-gotpl-lsp',
                    'Accept': 'application/json',
                },
                timeout: 8000, // 8-second timeout for slow connections
            };

            const req = https.get(PROXY_URL, options, (res) => {
                if (res.statusCode !== 200) {
                    res.resume();
                    resolve(null);
                    return;
                }
                let rawData = '';
                res.on('data', (chunk) => (rawData += chunk));
                res.on('end', () => {
                    try {
                        const parsed = JSON.parse(rawData);
                        resolve(parsed.Version || null); // e.g. "v0.5.0"
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
     * @param interactive - When true (manual command click), shows progress and feedback dialogs.
     */
    static async checkForUpdates(
        analyzerPath: string,
        outputChannel: vscode.OutputChannel,
        interactive = false
    ): Promise<void> {
        const runCheck = async () => {
            try {
                const [installed, latest] = await Promise.all([
                    this.getInstalledVersion(analyzerPath),
                    this.getLatestRemoteVersion(),
                ]);

                outputChannel.appendLine(`[Installer] Version check: installed=${installed ?? 'unknown'}, latest=${latest ?? 'unknown'}`);

                if (!installed || !latest) {
                    outputChannel.appendLine(`[Installer] Skipping update check — could not resolve version information.`);
                    if (interactive) {
                        vscode.window.showWarningMessage('Could not retrieve gotpl-analyzer release info from proxy.golang.org.');
                    }
                    return;
                }

                if (this.isNewerVersion(installed, latest)) {
                    const updateItem = 'Update Now';
                    const selection = await vscode.window.showInformationMessage(
                        `A new version of gotpl-analyzer is available (${installed} → ${latest}).`,
                        updateItem
                    );

                    if (selection === updateItem) {
                        await this.installAnalyzer(outputChannel);
                    }
                } else if (interactive) {
                    await vscode.window.showInformationMessage(`gotpl-analyzer is up to date (${installed}).`);
                }
            } catch (err) {
                outputChannel.appendLine(`[Installer] Update check failed: ${err}`);
                if (interactive) {
                    vscode.window.showErrorMessage(`Update check failed: ${err}`);
                }
            }
        };

        if (interactive) {
            await vscode.window.withProgress({
                location: vscode.ProgressLocation.Notification,
                title: 'Checking for gotpl-analyzer updates...',
                cancellable: false,
            }, runCheck);
        } else {
            await runCheck();
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
     * SemVer comparison (returns true if remote > current).
     */
    private static isNewerVersion(current: string, remote: string): boolean {
        const parse = (v: string) =>
            v.replace(/^v/, '').split('-')[0].split('.').map((n) => parseInt(n, 10) || 0);

        const [cMajor = 0, cMinor = 0, cPatch = 0] = parse(current);
        const [rMajor = 0, rMinor = 0, rPatch = 0] = parse(remote);

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
            // Check for updates in background on startup (non-interactive)
            void this.checkForUpdates(analyzerPath, outputChannel, false);
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

    /**
     * Resolves the executable binary path with comprehensive fallback checks.
     */
    static async getAnalyzerPath(): Promise<string | null> {
        // 1. Check user-configured path
        const configPath = vscode.workspace.getConfiguration('gotpl').get<string>('goAnalyzerPath');
        if (configPath && fs.existsSync(configPath)) {
            return configPath;
        }

        // 2. Check GOPATH/bin
        const goBin = await this.getGoBinPath();
        if (goBin) {
            const binPath = path.join(goBin, BINARY_NAME);
            if (fs.existsSync(binPath)) return binPath;
        }

        // 3. Check default ~/go/bin
        const homeDir = process.env.HOME || process.env.USERPROFILE;
        if (homeDir) {
            const defaultGoBin = path.join(homeDir, 'go', 'bin', BINARY_NAME);
            if (fs.existsSync(defaultGoBin)) return defaultGoBin;
        }

        // 4. Check system PATH via which/where
        try {
            const command = process.platform === 'win32' ? 'where' : 'which';
            const { stdout } = await exec(`${command} ${BINARY_NAME}`);
            const resolved = stdout.trim().split('\n')[0]?.trim();
            if (resolved && fs.existsSync(resolved)) return resolved;
        } catch { }

        return null;
    }
}
