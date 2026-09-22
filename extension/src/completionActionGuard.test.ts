import { join } from 'path';

class Range {
    constructor(public startLine: number, public startChar: number, public endLine: number, public endChar: number) {}
}
class CompletionItem {
    detail = '';
    documentation: any;
    range: any;
    constructor(public label: string, public kind?: number) {}
}
class MarkdownString {
    constructor(public value: string) {}
}

jest.mock('vscode', () => ({
    Range,
    CompletionItem,
    MarkdownString,
    CompletionItemKind: { Field: 5, Method: 2, Value: 12, File: 17, Function: 3, Variable: 6 },
    workspace: {
        getWorkspaceFolder: () => undefined,
        workspaceFolders: undefined,
        getConfiguration: () => ({ get: () => undefined }),
        textDocuments: [],
    },
}), { virtual: true });

import { TemplateParser } from './templateParser';
import { ScopeUtils } from './scopeUtils';
import { CompletionProvider } from './completionProvider';
import { TemplateContext, TemplateVar } from './types';

function makeDocument(content: string) {
    const lines = content.split('\n');
    return {
        getText: () => content,
        lineAt: (line: number) => ({ text: lines[line] ?? '' }),
        offsetAt: (pos: { line: number; character: number }) => {
            let offset = 0;
            for (let i = 0; i < pos.line; i++) offset += lines[i].length + 1;
            return offset + pos.character;
        },
        uri: { fsPath: '/virtual/templates/views/page.html' },
    } as any;
}

function makeProvider() {
    const parser = new TemplateParser();
    const graphBuilder = {
        getGraph: () => ({
            funcMaps: new Map(),
            namedBlocks: new Map(),
            templates: new Map(),
            typeRegistry: new Map(),
            globalTypeIndex: new Map(),
        }),
    } as any;
    const scope = new ScopeUtils(parser, graphBuilder);
    return new CompletionProvider(graphBuilder, scope, {} as any);
}

const vars = new Map<string, TemplateVar>([
    ['Title', { name: 'Title', type: 'string', isSlice: false }],
    ['user', {
        name: 'user', type: 'handlers.User', isSlice: false,
        fields: [{ name: 'Name', type: 'string', isSlice: false }],
    }],
]);

const ctx: TemplateContext = {
    templatePath: 'views/page.html',
    absolutePath: '/virtual/templates/views/page.html',
    vars,
    renderCalls: [],
};

test('no completion outside an action', () => {
    const provider = makeProvider();

    // Plain text: a literal "." is not a template expression.
    const plainDot = '<html>\n.\n</html>';
    expect(provider.getCompletions(makeDocument(plainDot), { line: 1, character: 1 } as any, ctx)).toEqual([]);

    // Plain text: HTML attribute quote should not offer template names.
    const plainQuote = '<a class="';
    expect(provider.getCompletions(makeDocument(plainQuote), { line: 0, character: 10 } as any, ctx)).toEqual([]);

    // Plain text: a "$" in JS/CSS should not offer locals/globals.
    const plainDollar = '<script>const x = $';
    expect(provider.getCompletions(makeDocument(plainDollar), { line: 0, character: 17 } as any, ctx)).toEqual([]);
});

test('completion still works inside an action', () => {
    const provider = makeProvider();

    // Bare dot inside an action → root scope vars.
    const bareDot = '{{ . }}';
    const items = provider.getCompletions(makeDocument(bareDot), { line: 0, character: 4 } as any, ctx);
    expect(items.map(i => i.label)).toEqual(expect.arrayContaining(['Title', 'user']));

    // Dot-path inside an action → struct fields.
    const dotPath = '{{ .user. }}';
    const fieldItems = provider.getCompletions(makeDocument(dotPath), { line: 0, character: 9 } as any, ctx);
    expect(fieldItems.map(i => i.label)).toContain('Name');
});

test('multi-line action is recognised as inside', () => {
    const provider = makeProvider();
    const content = '{{ printf\n  . }}';
    const items = provider.getCompletions(makeDocument(content), { line: 1, character: 3 } as any, ctx);
    expect(items.map(i => i.label)).toEqual(expect.arrayContaining(['Title', 'user']));
});
