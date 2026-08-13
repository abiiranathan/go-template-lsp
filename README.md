# Go Template LSP (Language Server Protocol)

![LOGO](./extension/logo.png)

Bring the power of Go's static typing directly into your HTML and text templates.

**Go Template LSP** is a high-performance VS Code extension and standalone Go analyzer that provides real-time type-safe validation, sub-millisecond IntelliSense, rich documentation hovers, and seamless cross-file navigation for Go `html/template` and `text/template` files.

If you've ever been frustrated by discovering template typos, missing variables, invalid field accesses, or type mismatches only *after* compiling and running your server, this extension is for you.

---

## 🚀 Key Features

*   **Framework Agnostic**: Works out-of-the-box with any Go web framework (Fiber, Echo, Gin, Chi, standard library `net/http`) using standard render patterns (e.g., `c.Render(name, data)` or `tmpl.ExecuteTemplate(wr, name, data)`).
*   **Live Type-Safe Validation**: Detects missing variables, undefined struct fields, invalid map lookups, and type mismatches as you type.
*   **Instant Sub-Millisecond Hover & IntelliSense**:
    *   Hover over any `{{ .Variable }}`, struct field, method, or function to view its exact Go type, parameter/return signatures, GoDoc comments, and nested field structures.
    *   Smart autocomplete suggestions for struct fields, methods, top-level variables, block locals (`$var`), built-in template functions, and user-registered `FuncMap` functions.
*   **Deep Scope & Context Tracking**: Accurately tracks the `.` (dot) context across complex nested structures:
    *   `{{ range }}`, `{{ with }}`, and `{{ if }}` blocks.
    *   Extended `else` branches: `{{ else if }}`, `{{ else with $v := . }}`, and `{{ else range $k, $v := . }}`.
    *   Local variable declarations and multi-variable assignments (`{{ $x := .Name }}`, `{{ $k, $v := .Map }}`).
*   **Cross-File Block Resolution**: Full awareness of `{{ define "name" }}` and `{{ block "name" . }}` across the entire workspace. Aggregates call-site arguments from parent templates to provide type inference and autocomplete inside shared partial files and detached define blocks.
*   **Built-in & Custom FuncMap Support**: Full type inference for Go template built-ins (`len`, `index`, `slice`, `print`, `printf`, `println`, `html`, `js`, `urlquery`, `eq`, `ne`, `lt`, `gt`, boolean logic, arithmetic) and custom functions injected via `template.FuncMap`.
*   **First-Class `dict` Helper Support**: Native evaluation of `(dict "key" .Value)` calls, generating on-the-fly typed structures for partials receiving composite dictionaries.
*   **Seamless Two-Way Navigation**:
    *   **Go to Definition (Template $\to$ Go)**: Jump directly from a template variable or field to its Go struct declaration in source code (`.go`).
    *   **Go to Definition (Template $\to$ Template)**: Jump from a `{{ template "name" }}` call directly to its `{{ define "name" }}` declaration.
    *   **Go to Definition (Go $\to$ Template)**: Click on a template name inside a Go handler's `Render("views/index.html", data)` call to open the template file.
    *   **Find References**: Discover every template and block call-site across the entire workspace.
*   **Interactive Knowledge Graph**: Visualize relationships between Go handlers, template files, data contexts, and variables in an interactive webview panel.

---

## ⚡ Performance Architecture

The extension is architected for zero-latency interactive feedback:

1. **In-Memory Sub-Millisecond Resolution**: Hover, autocompletion, and go-to-definition execute entirely in-memory using an optimized AST and a pre-indexed global type registry.
2. **AST Parse Caching**: A bounded LRU cache in `TemplateParser` prevents redundant parsing of unchanged document content during cursor movements and hovers.
3. **Pre-Indexed Partial Call Map (`partialToParents`)**: When analyzing partials or detached blocks, the extension queries an upfront call graph index instead of crawling and parsing disk files sequentially.
4. **Persistent Go Daemon**: A long-lived Go analyzer process runs in the background communicating over stdio JSON-RPC, keeping type metadata resident and ready for live re-validations.
5. **Incremental Template Re-Validation**: Template edits skip full Go AST re-analysis and reuse cached Go type declarations, re-validating in milliseconds.
6. **O(1) Workspace Lookups**: Absolute file paths and named blocks are indexed into hash maps for constant-time context retrieval.

---

## 🛠 Installation & Usage

The extension automatically activates when opening a workspace with Go files and templates (`.html`, `.tmpl`, `.gohtml`, `.tpl`).

### Analyzer Installation
The extension will automatically check for and prompt you to install the analyzer tool on startup. To install or update manually:

```bash
go install github.com/abiiranathan/go-template-lsp/gotpl-analyzer@latest
```

### Available Commands
Open the Command Palette (`Ctrl+Shift+P` / `Cmd+Shift+P`) and type `GoTpl`:

*   **GoTpl: Rebuild Template Index**: Forces a full re-analysis of Go source files to discover new handlers, templates, types, and FuncMaps.
*   **GoTpl: Validate Current Template**: Manually triggers validation diagnostics on the active editor file.
*   **GoTpl: Show Template Knowledge Graph**: Opens an interactive visual panel detailing all templates, handler call-sites, and variable structures.

---

## ⚙️ Configuration

Customize the extension behavior in your VS Code `settings.json`:

```jsonc
{
  // Enable or disable the extension features
  "gotpl.enabled": true,

  // Custom path to the gotpl-analyzer binary.
  "gotpl.goAnalyzerPath": "",

  // Go source directory relative to the workspace root
  "gotpl.sourceDir": ".",

  // Root directory for templates relative to sourceDir or templateBaseDir
  "gotpl.templateRoot": "views",

  // Base directory for templates (if located outside sourceDir)
  "gotpl.templateBaseDir": "",

  // Path to an optional JSON file with global context variables (e.g. middleware variables)
  "gotpl.contextFile": "",

  // Debounce delay in milliseconds before live diagnostics trigger on edit
  "gotpl.debounceMs": 800,

  // Enable or disable live template validation diagnostics
  "gotpl.validate": true,

  // Enable GZIP compression between the Go analyzer daemon and the editor (useful for massive codebases)
  "gotpl.compress": false,

  // Show validation errors at partial call sites in addition to the source definition site
  "gotpl.showCallSiteErrors": false
}
```

---

## 💻 Standalone CLI Tool (CI/CD Quality Gate)

The core analyzer engine can run in standalone CLI mode to validate all templates in continuous integration pipelines:

```bash
# Install analyzer
go install github.com/abiiranathan/go-template-lsp/gotpl-analyzer@latest

# Run validation across your project
gotpl-analyzer -dir . -template-root views -validate
```

### CLI Flags
```text
Usage of gotpl-analyzer:
  -dir string
        Go source directory to analyze (default ".")
  -template-root string
        Root directory for templates (relative to dir or template-base-dir)
  -template-base-dir string
        Base directory for template-root (if different from -dir)
  -validate
        Validate template files against discovered Go render calls
  -context-file string
        Path to JSON file containing additional global context variables
  -compress
        Output gzip-compressed JSON responses
  -daemon
        Run as a long-lived JSON-RPC daemon over stdio
  -named-templates
        Output all discovered named templates and blocks as JSON
  -view-context string
        Print resolved variable context for a specific template path
```

---

## 🏗 Technical Architecture

```text
┌──────────────────────────────────────────────────────────────────┐
│                   VS Code Extension (TypeScript)                 │
│                                                                  │
│  ┌───────────────────────┐             ┌──────────────────────┐  │
│  │   TemplateParser      │             │ KnowledgeGraphBuilder│  │
│  │  (LRU AST Caching)    │             │ (Contexts & Types)   │  │
│  └───────────┬───────────┘             └──────────┬───────────┘  │
│              │                                    │              │
│  ┌───────────▼───────────┐             ┌──────────▼───────────┐  │
│  │     ScopeUtils        │             │   TypeInferencer     │  │
│  │(Scope & Dot Tracking) │             │ (Expression Engine)  │  │
│  └───────────┬───────────┘             └──────────┬───────────┘  │
│              │                                    │              │
│  ┌───────────▼────────────────────────────────────▼───────────┐  │
│  │  Providers: Hover | Completion | Definition | References   │  │
│  └───────────────────────────────┬────────────────────────────┘  │
└──────────────────────────────────┼───────────────────────────────┘
                                   │
                     JSON-RPC 2.0  │ (Stdio IPC)
                     Async Fallback│ Live Diagnostics
                                   │
┌──────────────────────────────────▼───────────────────────────────┐
│                     Go Analyzer Daemon (Go)                      │
│                                                                  │
│  ┌───────────────────────┐             ┌──────────────────────┐  │
│  │ go/types & go/parser  │             │   RenderCall Miner   │  │
│  │ (Full Go Type Engine) │             │ (AST Handler Search) │  │
│  └───────────────────────┘             └──────────────────────┘  │
└──────────────────────────────────────────────────────────────────┘
```

### Core Client Subsystems

*   **`KnowledgeGraphBuilder`**: Constructs and maintains the project-wide call graph connecting Go handlers, render calls, template files, named blocks, FuncMaps, and global struct definitions.
*   **`TemplateParser`**: High-speed recursive descent parser outputting nested AST nodes with full support for Go template block semantics (`range`, `with`, `if`, `block`, `define`, `assignment`).
*   **`TypeInferencer`**: Expression engine capable of evaluating chained method invocations, pipeline stages (`.Data | func`), slice indexing, map key access, pointer stripping, and operator return types.
*   **`ScopeUtils`**: Manages cursor-position scope stacks, resolving lexical variable scopes, local identifiers (`$var`), and block-inherited context frames.

---

## 🛠 Development & Contributing

### Prerequisites
*   **Go**: 1.22+
*   **Node.js**: 18.x or 20.x
*   **npm** or **pnpm**

### Quick Start
1. Clone the repository:
   ```bash
   git clone https://github.com/abiiranathan/go-template-lsp.git
   cd go-template-lsp
   ```
2. Install extension dependencies:
   ```bash
   npm install
   ```
3. Run test suites:
   ```bash
   npm test
   ```
4. Launch Extension Development Host:
   * Press `F5` in VS Code to start a development window with the extension loaded.

---

## 📄 License

MIT © [Dr. Abiira Nathan](https://github.com/abiiranathan)