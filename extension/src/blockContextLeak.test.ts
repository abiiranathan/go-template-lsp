import { join } from 'path';

jest.mock('vscode', () => ({
    Range: class {},
    CompletionItem: class {},
    CompletionItemKind: { Field: 5, Method: 2, Value: 12, File: 17, Function: 3, Variable: 6 },
    MarkdownString: class {},
    workspace: {
        getWorkspaceFolder: () => undefined,
        workspaceFolders: undefined,
        getConfiguration: () => ({ get: () => undefined }),
        textDocuments: [],
    },
}), { virtual: true });

import { KnowledgeGraphBuilder } from './knowledgeGraph';
import { AnalysisResult } from './types';

const ROOT = '/virtual/workspace';

/**
 * Regression: an inline named block invoked with a narrowed "." context (here
 * "." = handlers.Drug from inside {{ range .billedDrugs }}) must not leak that
 * context into the root variables of the file that defines it. The analyzer
 * emits the block's context as a synthetic "template-call" render call; the
 * extension used to merge every such call into the block's defining file.
 */
test('synthetic block call does not leak "." into a directly-rendered file', () => {
    const analysis: AnalysisResult = {
        errors: [],
        renderCalls: [
            {
                file: 'handler.go',
                line: 1,
                template: 'views/tc.html',
                templateNameStartCol: 0,
                templateNameEndCol: 0,
                vars: [
                    { name: 'billedDrugs', type: '[]handlers.Drug', isSlice: true, elemType: 'handlers.Drug' },
                    { name: 'Title', type: 'string', isSlice: false },
                ],
            },
            // Synthetic call the analyzer emits for the block body.
            {
                file: 'template-call',
                line: 1,
                template: 'billed-drug',
                templateNameStartCol: 0,
                templateNameEndCol: 0,
                vars: [
                    { name: '.', type: 'handlers.Drug', isSlice: false },
                ],
            },
            // A real render call targeting a named block must still propagate
            // its context to the block's defining file.
            {
                file: 'handler.go',
                line: 2,
                template: 'partial',
                templateNameStartCol: 0,
                templateNameEndCol: 0,
                vars: [{ name: 'visit', type: 'handlers.Visit', isSlice: false }],
            },
        ],
        namedBlocks: {
            'billed-drug': [{
                name: 'billed-drug',
                absolutePath: join(ROOT, 'templates', 'views', 'tc.html'),
                templatePath: 'views/tc.html',
                line: 10,
                col: 1,
            }],
            'partial': [{
                name: 'partial',
                absolutePath: join(ROOT, 'templates', 'views', 'partials', 'partial.html'),
                templatePath: 'views/partials/partial.html',
                line: 1,
                col: 1,
            }],
        },
    };

    const gb = new KnowledgeGraphBuilder(
        ROOT,
        { appendLine: () => {} } as any,
        { text: '', show: () => {}, hide: () => {} } as any,
    );
    gb.build(analysis);
    const graph = gb.getGraph();

    // Root of the directly-rendered file must not contain the block's "." context.
    const tc = graph.templates.get('views/tc.html');
    expect(tc).toBeDefined();
    expect(tc!.vars.has('.')).toBe(false);
    expect([...tc!.vars.keys()]).toEqual(expect.arrayContaining(['billedDrugs', 'Title']));

    // The block body keeps its own context for hover/validation inside it.
    const block = graph.templates.get('billed-drug');
    expect(block).toBeDefined();
    expect(block!.vars.get('.')?.type).toBe('handlers.Drug');

    // Real render calls to a block name still populate the defining file.
    const partialFile = graph.templates.get('views/partials/partial.html');
    expect(partialFile).toBeDefined();
    expect(partialFile!.vars.has('visit')).toBe(true);
});
