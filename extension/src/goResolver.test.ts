import { GoResolver, ExecFn, FsLike } from './goResolver';

/**
 * Builds a fake FsLike backed by a Set of paths considered to exist.
 */
function fakeFs(existingPaths: string[]): FsLike {
    const set = new Set(existingPaths);
    return {
        existsSync: (p: string) => set.has(p),
    };
}

/**
 * Builds a fake ExecFn. `handlers` maps a command string to either a
 * stdout string (success) or an Error (rejection). Falls through to
 * rejecting with "command not found" for anything unmapped, mirroring
 * real shell behavior for a missing binary.
 */
function fakeExec(handlers: Record<string, string | Error>): ExecFn {
    const calls: string[] = [];
    const fn = (async (command: string) => {
        calls.push(command);
        const handler = handlers[command];
        if (handler === undefined) {
            throw new Error(`command not found: ${command}`);
        }
        if (handler instanceof Error) {
            throw handler;
        }
        return { stdout: handler, stderr: '' };
    }) as ExecFn & { calls: string[] };
    fn.calls = calls;
    return fn;
}


describe('GoResolver.resolveGoBinary', () => {
    it('prefers an explicitly configured path when it exists on disk', async () => {
        const resolver = new GoResolver({
            configuredPath: '/custom/go/bin/go',
            fs: fakeFs(['/custom/go/bin/go']),
            exec: fakeExec({}), // should not even be called
            platform: 'posix',
        });

        const result = await resolver.resolveGoBinary();

        expect(result).toEqual({ binaryPath: '/custom/go/bin/go', source: 'configured' });
    });

    it('ignores a configured path that does not exist and falls through to PATH', async () => {
        const resolver = new GoResolver({
            configuredPath: '/does/not/exist/go',
            fs: fakeFs(['/usr/local/go/bin/go']),
            exec: fakeExec({ 'which go': '/usr/local/go/bin/go\n' }),
            platform: 'posix',
        });

        const result = await resolver.resolveGoBinary();

        expect(result).toEqual({ binaryPath: '/usr/local/go/bin/go', source: 'path' });
    });

    it('resolves via `which go` on posix when PATH is correct', async () => {
        const resolver = new GoResolver({
            fs: fakeFs(['/opt/homebrew/bin/go']),
            exec: fakeExec({ 'which go': '/opt/homebrew/bin/go\n' }),
            platform: 'posix',
        });

        const result = await resolver.resolveGoBinary();

        expect(result).toEqual({ binaryPath: '/opt/homebrew/bin/go', source: 'path' });
    });

    it('resolves via `where go` on win32', async () => {
        const resolver = new GoResolver({
            fs: fakeFs(['C:\\Go\\bin\\go.exe']),
            exec: fakeExec({ 'where go': 'C:\\Go\\bin\\go.exe\r\n' }),
            platform: 'win32',
        });

        const result = await resolver.resolveGoBinary();

        expect(result).toEqual({ binaryPath: 'C:\\Go\\bin\\go.exe', source: 'path' });
    });

    it('reproduces the reported bug: `which go` throws "not found" despite go being installed, then recovers via a known location', async () => {
        // This is the exact failure mode from the bug report: go is
        // installed and on the user's shell PATH, but the extension
        // host's child process does not inherit it, so `which go`
        // itself fails with a non-zero exit / ENOENT-style error.
        const resolver = new GoResolver({
            fs: fakeFs(['/usr/local/go/bin/go']),
            exec: fakeExec({ 'which go': new Error("/bin/sh: 1: which: not found") }),
            platform: 'posix',
        });

        const result = await resolver.resolveGoBinary();

        expect(result).toEqual({ binaryPath: '/usr/local/go/bin/go', source: 'known-location' });
    });

    it('falls through to GOROOT when PATH and known locations both miss', async () => {
        const resolver = new GoResolver({
            fs: fakeFs(['/opt/go-custom/bin/go']),
            exec: fakeExec({}), // which go fails (unmapped -> throws)
            env: { GOROOT: '/opt/go-custom' },
            platform: 'posix',
        });

        const result = await resolver.resolveGoBinary();

        expect(result).toEqual({ binaryPath: '/opt/go-custom/bin/go', source: 'goroot' });
    });

    it('returns not-found when nothing resolves', async () => {
        const resolver = new GoResolver({
            fs: fakeFs([]),
            exec: fakeExec({}),
            env: {},
            platform: 'posix',
        });

        const result = await resolver.resolveGoBinary();

        expect(result).toEqual({ binaryPath: null, source: 'not-found' });
    });

    it('caches the result and does not re-invoke exec on a second call', async () => {
        const exec = fakeExec({ 'which go': '/usr/local/go/bin/go\n' }) as ExecFn & { calls: string[] };
        const resolver = new GoResolver({
            fs: fakeFs(['/usr/local/go/bin/go']),
            exec,
            platform: 'posix',
        });

        await resolver.resolveGoBinary();
        await resolver.resolveGoBinary();

        expect(exec.calls.length).toBe(1);
    });

    it('bypasses the cache when forceRefresh is true', async () => {
        const exec = fakeExec({ 'which go': '/usr/local/go/bin/go\n' }) as ExecFn & { calls: string[] };
        const resolver = new GoResolver({
            fs: fakeFs(['/usr/local/go/bin/go']),
            exec,
            platform: 'posix',
        });

        await resolver.resolveGoBinary();
        await resolver.resolveGoBinary(true);

        expect(exec.calls.length).toBe(2);
    });
});

describe('GoResolver.buildChildEnv', () => {
    it('prepends the go binary directory to PATH on posix', () => {
        const resolver = new GoResolver({
            env: { PATH: '/usr/bin:/bin' },
            platform: 'posix',
        });

        const env = resolver.buildChildEnv('/usr/local/go/bin/go');

        expect(env.PATH).toBe('/usr/local/go/bin:/usr/bin:/bin');
    });

    it('prepends to Path (capitalized) on win32', () => {
        const resolver = new GoResolver({
            env: { Path: 'C:\\Windows\\System32' },
            platform: 'win32',
        });

        const env = resolver.buildChildEnv('C:\\Go\\bin\\go.exe');

        expect(env.Path).toBe('C:\\Go\\bin;C:\\Windows\\System32');
    });

    it('handles an empty existing PATH without producing a leading delimiter artifact issue', () => {
        const resolver = new GoResolver({
            env: {},
            platform: 'posix',
        });

        const env = resolver.buildChildEnv('/usr/local/go/bin/go');

        expect(env.PATH).toBe('/usr/local/go/bin:');
    });
});

describe('GoResolver.run', () => {
    it('returns null without throwing when go cannot be resolved', async () => {
        const resolver = new GoResolver({
            fs: fakeFs([]),
            exec: fakeExec({}),
            env: {},
            platform: 'posix',
        });

        const result = await resolver.run(['env', 'GOPATH']);

        expect(result).toBeNull();
    });

    it('quotes the resolved binary path and joins args when go is found', async () => {
        const exec = fakeExec({
            'which go': '/usr/local/go/bin/go\n',
            '"/usr/local/go/bin/go" env GOPATH': '/home/user/go\n',
        }) as ExecFn & { calls: string[] };

        const resolver = new GoResolver({
            fs: fakeFs(['/usr/local/go/bin/go']),
            exec,
            env: { PATH: '/usr/bin' },
            platform: 'posix',
        });

        const result = await resolver.run(['env', 'GOPATH']);

        expect(result).toEqual({ stdout: '/home/user/go\n', stderr: '' });
        expect(exec.calls).toContain('"/usr/local/go/bin/go" env GOPATH');
    });

    it('handles a path containing spaces safely via quoting', async () => {
        const exec = fakeExec({
            'where go': 'C:\\Program Files\\Go\\bin\\go.exe\r\n',
            '"C:\\Program Files\\Go\\bin\\go.exe" version': 'go version go1.24.0\n',
        }) as ExecFn & { calls: string[] };

        const resolver = new GoResolver({
            fs: fakeFs(['C:\\Program Files\\Go\\bin\\go.exe']),
            exec,
            platform: 'win32',
        });

        const result = await resolver.run(['version']);

        expect(result?.stdout).toContain('go1.24.0');
        expect(exec.calls[exec.calls.length - 1]).toBe('"C:\\Program Files\\Go\\bin\\go.exe" version');
    });

    it('propagates an error thrown by the resolved go command itself', async () => {
        const resolver = new GoResolver({
            fs: fakeFs(['/usr/local/go/bin/go']),
            exec: fakeExec({
                'which go': '/usr/local/go/bin/go\n',
                '"/usr/local/go/bin/go" install bad/module@latest': new Error('module not found'),
            }),
            platform: 'posix',
        });

        await expect(resolver.run(['install', 'bad/module@latest'])).rejects.toThrow('module not found');
    });
});

describe('GoResolver.quote', () => {
    it('wraps a path in double quotes', () => {
        expect(GoResolver.quote('/usr/local/go/bin/go')).toBe('"/usr/local/go/bin/go"');
    });

    it('wraps a space-containing Windows path in double quotes', () => {
        expect(GoResolver.quote('C:\\Program Files\\Go\\bin\\go.exe')).toBe('"C:\\Program Files\\Go\\bin\\go.exe"');
    });
});

