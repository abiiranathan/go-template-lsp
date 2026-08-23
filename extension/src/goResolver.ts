import * as path from 'path';
import * as fs from 'fs';
import * as cp from 'child_process';
import * as util from 'util';

/**
 * Minimal shape of `child_process.exec`'s promisified return value that
 * this module depends on. Matches `util.promisify(child_process.exec)`.
 */
export interface ExecResult {
    /** Captured stdout from the child process. */
    stdout: string;
    /** Captured stderr from the child process. */
    stderr: string;
}

/**
 * Signature of an exec-like function. `util.promisify(child_process.exec)`
 * satisfies this directly; tests can substitute a stub or spy instead.
 */
export type ExecFn = (command: string, options?: cp.ExecOptions) => Promise<ExecResult>;

/** Default exec implementation, backed by the real `child_process.exec`. */
const defaultExec: ExecFn = util.promisify(cp.exec) as unknown as ExecFn;

/** Minimal filesystem surface this module depends on, for test injection. */
export interface FsLike {
    /** Synchronously reports whether a path exists on disk. */
    existsSync(p: string): boolean;
}

const defaultFs: FsLike = fs;

/** Platform identifiers this module distinguishes behavior for. */
export type Platform = 'win32' | 'posix';

/**
 * Optional dependencies for GoResolver, all defaulted to real
 * implementations. Overriding these is the seam tests use to avoid
 * touching the real filesystem, spawning real processes, or depending
 * on the host OS.
 */
export interface GoResolverOptions {
    /** Exec implementation to run shell commands. Defaults to child_process.exec. */
    exec?: ExecFn;
    /** Filesystem implementation used for existence checks. Defaults to node:fs. */
    fs?: FsLike;
    /** Process environment snapshot to read PATH/GOROOT/HOME from. Defaults to process.env. */
    env?: NodeJS.ProcessEnv;
    /** Platform to branch on. Defaults to node's process.platform, normalized to 'win32' | 'posix'. */
    platform?: Platform;
    /** Optional sink for diagnostic messages. Defaults to a no-op. */
    log?: (message: string) => void;
    /** Optional explicit override path, e.g. from a user setting. Checked first if provided. */
    configuredPath?: string | null;
}

/** Resolution outcome, distinguishing "not found" from "found via X" for logging/testing. */
export interface ResolveResult {
    /** Absolute path to the resolved go binary, or null if none was found. */
    binaryPath: string | null;
    /** Which resolution strategy produced the result, for diagnostics and tests. */
    source: 'configured' | 'path' | 'login-shell' | 'known-location' | 'goroot' | 'not-found';
}

/**
 * Resolves the absolute path to the `go` executable and runs go
 * commands with a corrected child environment.
 *
 * Exists as a standalone class (rather than static methods coupled to
 * VS Code) specifically so it can be constructed with fake exec/fs/env
 * in unit tests, and reused from contexts other than the VS Code
 * extension host (CLI tools, other extensions, etc.).
 *
 * Not safe for concurrent resolveGoBinary() calls with forceRefresh
 * races in mind — callers doing highly concurrent resolution should
 * await one resolution before starting another, or accept redundant
 * lookups (harmless beyond wasted work).
 */
export class GoResolver {
    private readonly exec: ExecFn;
    private readonly fs: FsLike;
    private readonly env: NodeJS.ProcessEnv;
    private readonly platform: Platform;
    private readonly log: (message: string) => void;
    private readonly configuredPath: string | null;

    /** Cached resolution so repeated calls don't re-run the full search. */
    private cached: ResolveResult | undefined;

    constructor(options: GoResolverOptions = {}) {
        this.exec = options.exec ?? defaultExec;
        this.fs = options.fs ?? defaultFs;
        this.env = options.env ?? process.env;
        this.platform = options.platform ?? (process.platform === 'win32' ? 'win32' : 'posix');
        this.log = options.log ?? (() => { });
        this.configuredPath = options.configuredPath ?? null;
    }

    /** Name of the go executable for the target platform. */
    private goBinaryName(): string {
        return this.platform === 'win32' ? 'go.exe' : 'go';
    }

    /**
     * Wraps a path in double quotes for safe interpolation into a shell
     * command string, guarding against spaces (e.g. "Program Files").
     */
    static quote(p: string): string {
        return `"${p}"`;
    }

    /**
     * Common install locations that the extension host's inherited PATH
     * often misses, ordered by likelihood.
     */
    private knownCandidates(): string[] {
        if (this.platform === 'win32') {
            const userProfile = this.env.USERPROFILE ?? '';
            return [
                'C:\\Go\\bin\\go.exe',
                path.win32.join(this.env.LOCALAPPDATA ?? '', 'Programs', 'Go', 'bin', 'go.exe'),
                path.win32.join(this.env.ProgramFiles ?? 'C:\\Program Files', 'Go', 'bin', 'go.exe'),
                // Scoop / user-local installs
                path.win32.join(userProfile, 'scoop', 'apps', 'go', 'current', 'bin', 'go.exe'),
                path.win32.join(userProfile, 'go', 'bin', 'go.exe'),
            ];
        }
        const home = this.env.HOME ?? '';
        return [
            '/usr/local/go/bin/go',   // official Go tarball install (Linux/macOS)
            '/opt/homebrew/bin/go',   // Homebrew on Apple Silicon
            '/usr/local/bin/go',      // Homebrew on Intel Mac / some Linux distros
            '/usr/bin/go',            // Linux distro packages (apt/dnf/pacman)
            '/snap/bin/go',           // Go installed via snap
            '/home/linuxbrew/.linuxbrew/bin/go', // Homebrew on Linux
            // Version-manager shims (PATH for these is usually only set in
            // interactive shell RC files the extension host never sources).
            path.posix.join(home, '.asdf', 'shims', 'go'),
            path.posix.join(home, '.local', 'share', 'mise', 'shims', 'go'),
            path.posix.join(home, '.goenv', 'shims', 'go'),
            path.posix.join(home, 'go', 'bin', 'go'),
        ];
    }

    /**
     * Resolves the absolute path to the `go` executable.
     *
     * A shell's inherited PATH (login profile, version managers such as
     * asdf/gvm/mise, or a stale process env on Windows) is frequently
     * NOT visible to the calling process — the classic symptom is
     * `exec('go ...')` failing with "'go' file not found" even though
     * `go` works fine in the user's terminal.
     *
     * Resolution order:
     *   1. `configuredPath` passed to the constructor, if it exists on disk.
     *   2. `which go` / `where go` against the current process env.
     *   3. The user's login shell, asked directly for go's location
     *      (`$SHELL -lic 'command -v go'`) — this sees version-manager
     *      shims and profile PATH edits the extension host cannot.
     *   4. Common OS-specific install locations.
     *   5. `$GOROOT/bin/go`, if GOROOT is set in the given env.
     *
     * Successful results are cached; failures are NOT cached so a retry
     * (e.g. after the user installs Go) re-probes instead of replaying a
     * stale negative result. Pass `forceRefresh: true` to bypass the cache.
     */
    async resolveGoBinary(forceRefresh = false): Promise<ResolveResult> {
        if (!forceRefresh && this.cached !== undefined) {
            return this.cached;
        }

        // 1. Explicit override, e.g. a user setting passed in by the caller.
        if (this.configuredPath && this.fs.existsSync(this.configuredPath)) {
            this.log(`Using configured go binary: ${this.configuredPath}`);
            return this.remember({ binaryPath: this.configuredPath, source: 'configured' });
        }

        // 2. Try PATH as this process sees it.
        try {
            const command = this.platform === 'win32' ? 'where go' : 'which go';
            const { stdout } = await this.exec(command, { env: this.env });
            const resolved = stdout.trim().split(/\r?\n/)[0]?.trim();
            if (resolved && this.fs.existsSync(resolved)) {
                this.log(`Resolved go binary via PATH: ${resolved}`);
                return this.remember({ binaryPath: resolved, source: 'path' });
            }
        } catch (err) {
            this.log(`'which/where go' failed against process PATH: ${err}`);
        }

        // 3. Ask the user's login shell. GUI-launched extension hosts do not
        // source .zprofile/.bashrc/.zshrc, which is where version managers
        // and custom toolchains add themselves to PATH.
        if (this.platform !== 'win32') {
            try {
                const shell = this.env.SHELL || '/bin/bash';
                const { stdout } = await this.exec(
                    `${shell} -lic 'command -v go'`,
                    { env: this.env, timeout: 8000 }
                );
                const resolved = stdout.trim().split(/\r?\n/)[0]?.trim();
                if (resolved && this.fs.existsSync(resolved)) {
                    this.log(`Resolved go binary via login shell (${shell}): ${resolved}`);
                    return this.remember({ binaryPath: resolved, source: 'login-shell' });
                }
            } catch (err) {
                this.log(`Login shell probe for go failed: ${err}`);
            }
        }

        // 4. Common install locations.
        for (const candidate of this.knownCandidates()) {
            if (candidate && this.fs.existsSync(candidate)) {
                this.log(`Resolved go binary via known install location: ${candidate}`);
                return this.remember({ binaryPath: candidate, source: 'known-location' });
            }
        }

        // 5. GOROOT, if defined.
        const goroot = this.env.GOROOT;
        if (goroot) {
            const platformPath = this.platform === 'win32' ? path.win32 : path.posix;
            const gorootBin = platformPath.join(goroot, 'bin', this.goBinaryName());
            if (this.fs.existsSync(gorootBin)) {
                this.log(`Resolved go binary via GOROOT: ${gorootBin}`);
                return this.remember({ binaryPath: gorootBin, source: 'goroot' });
            }
        }

        this.log('Could not resolve the go binary through any known method.');
        // Do NOT cache the failure — a later retry must re-probe.
        return { binaryPath: null, source: 'not-found' };
    }

    private remember(result: ResolveResult): ResolveResult {
        this.cached = result;
        return result;
    }

    /**
     * Builds a child process environment with the resolved go binary's
     * directory prepended to PATH, so subprocesses spawned by `go`
     * itself (git, a C compiler, etc.) can also be found.
     */
    buildChildEnv(goBinary: string): NodeJS.ProcessEnv {
        // Use the path implementation for the *target* platform, not the
        // host running this code — otherwise a win32-configured resolver
        // running in a test (or cross-platform tooling) on posix would
        // parse "C:\Go\bin\go.exe" with posix rules and mangle it.
        const platformPath = this.platform === 'win32' ? path.win32 : path.posix;
        const goDir = platformPath.dirname(goBinary);
        const pathKey = this.platform === 'win32' ? 'Path' : 'PATH';
        const delimiter = this.platform === 'win32' ? ';' : ':';
        const existingPath = this.env[pathKey] ?? '';
        return {
            ...this.env,
            [pathKey]: `${goDir}${delimiter}${existingPath}`,
        };
    }

    /**
     * Resolves the go binary and runs `<go> <args>` with a corrected
     * child environment. Returns null if go could not be resolved
     * (callers should surface their own error/UI in that case) instead
     * of throwing, so a missing toolchain is distinguishable from a
     * command that ran and failed.
     * @param args Arguments to append after the go binary, e.g. ['env', 'GOPATH'].
     * @param execOptions Additional exec options merged in (cwd, etc.); env is always overridden with buildChildEnv.
     * @throws Only if the resolved go binary itself fails (non-zero exit) — propagated from exec.
     */
    async run(args: string[], execOptions: cp.ExecOptions = {}): Promise<ExecResult | null> {
        const { binaryPath } = await this.resolveGoBinary();
        if (!binaryPath) {
            return null;
        }
        const command = `${GoResolver.quote(binaryPath)} ${args.join(' ')}`;
        return this.exec(command, {
            ...execOptions,
            env: this.buildChildEnv(binaryPath),
        });
    }
}
