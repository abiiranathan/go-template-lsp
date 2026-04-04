package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/abiiranathan/go-template-lsp/gotpl-analyzer/ast"
	"github.com/abiiranathan/go-template-lsp/gotpl-analyzer/validator"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      int64     `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type daemonAnalyzeParams struct {
	Dir             string `json:"dir"`
	TemplateRoot    string `json:"templateRoot"`
	TemplateBaseDir string `json:"templateBaseDir"`
	ContextFile     string `json:"contextFile"`
	Validate        bool   `json:"validate"`
}

type daemonValidateTemplateParams struct {
	AbsolutePath string `json:"absolutePath"`
	Content      string `json:"content"`
}

type daemonUpdateTemplateParams struct {
	AbsolutePath string `json:"absolutePath"`
	Content      string `json:"content"`
}

type daemonClearTemplateParams struct {
	AbsolutePath string `json:"absolutePath"`
}

type daemonInferExpressionParams struct {
	Expression  string                     `json:"expression"`
	Vars        map[string]ast.TemplateVar `json:"vars"`
	ScopeStack  []validator.ScopeType      `json:"scopeStack"`
	BlockLocals map[string]ast.TemplateVar `json:"blockLocals"`
}

type daemonGetHoverInfoParams struct {
	AbsolutePath string `json:"absolutePath"`
	Line         int    `json:"line"` // 1-based
	Col          int    `json:"col"`  // 1-based
	Content      string `json:"content"`
}

type daemonValidateTemplateResult struct {
	ValidationErrors []validator.ValidationResult `json:"validationErrors"`
	HasContext       bool                         `json:"hasContext"`
}

// daemonState is the immutable snapshot of analysis results shared by all
// concurrent read-only operations (validateTemplate, inferExpressionType,
// getHoverInfo).  The pointer is replaced atomically on each analyze call so
// readers always see a consistent snapshot without acquiring a write lock.
//
// OPTIMISATION: Previously every read-only handler performed deep clones of
// renderVarsByTemplate, funcMaps, typeRegistry, namedBlocks, and
// templateOverlays under a write-locked mutex — O(n) allocations per request.
// With an atomic pointer swap, read-only handlers simply load the pointer and
// read the shared snapshot.  Only the mutable templateOverlays map (written
// per file save) is still protected by a lightweight RWMutex.
type daemonState struct {
	dir          string
	baseDir      string
	templateRoot string
	contextFile  string
	validate     bool
	output       ValidationOutput

	renderVarsByTemplate map[string][]ast.TemplateVar
	funcMaps             validator.FuncMapRegistry
	typeRegistry         map[string][]ast.FieldInfo
	namedBlocks          map[string][]validator.NamedBlockEntry
	partialTargets       map[string]bool

	// goFingerprint is a hash of all Go file paths + modification times.
	// If unchanged between analyze calls, we can skip packages.Load entirely.
	goFingerprint string

	// templateFingerprint is a hash of all template file paths + modification times.
	// If unchanged, we can skip template re-validation too.
	templateFingerprint string

	// analysisResult is the raw ast.AnalysisResult kept for incremental re-validation.
	// When only templates change, we reuse this instead of re-running packages.Load.
	analysisResult *ast.AnalysisResult
}

type analyzerDaemon struct {
	// state is replaced atomically on analyze; read-only handlers load it with
	// atomic.Pointer.Load() which does not block.
	state atomic.Pointer[daemonState]

	// templateOverlays is the only field that mutates after analyze completes
	// (via updateTemplate / clearTemplate).  Protected by its own fine-grained
	// RWMutex instead of the coarse daemon-wide lock.
	overlayMu        sync.RWMutex
	templateOverlays map[string]string
}

// runDaemon starts a JSON-RPC 2.0 server over stdin/stdout that serves
// analysis, validation, hover, and type-inference requests. The daemon
// maintains an immutable snapshot of analysis results that is atomically
// swapped on each analyze call, enabling lock-free reads for concurrent
// read-only operations.
func runDaemon(stdin io.Reader, stdout io.Writer) error {
	server := &analyzerDaemon{
		templateOverlays: make(map[string]string),
	}
	reader := bufio.NewReader(stdin)
	writer := bufio.NewWriter(stdout)

	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}

		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			if err := writeResponse(writer, rpcResponse{
				JSONRPC: "2.0",
				Error:   &rpcError{Code: -32700, Message: fmt.Sprintf("invalid request: %v", err)},
			}); err != nil {
				return err
			}
			continue
		}

		resp := server.handle(req)
		if err := writeResponse(writer, resp); err != nil {
			return err
		}

		if req.Method == "shutdown" {
			return nil
		}
	}
}

func writeResponse(writer *bufio.Writer, resp rpcResponse) error {
	data, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	if _, err := writer.Write(append(data, '\n')); err != nil {
		return err
	}
	return writer.Flush()
}

// handle routes an incoming JSON-RPC request to the appropriate method handler.
// Supported methods: analyze, reanalyzeTemplates, validateTemplate, updateTemplate,
// clearTemplate, inferExpressionType, getHoverInfo, shutdown.
func (d *analyzerDaemon) handle(req rpcRequest) rpcResponse {
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	defer func() {
		if recovered := recover(); recovered != nil {
			resp.Error = &rpcError{Code: -32001, Message: fmt.Sprintf("daemon panic during %s: %v", req.Method, recovered)}
		}
	}()

	switch req.Method {
	case "analyze":
		var params daemonAnalyzeParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: fmt.Sprintf("invalid analyze params: %v", err)}
			return resp
		}
		result, err := d.analyze(params)
		if err != nil {
			resp.Error = &rpcError{Code: -32000, Message: err.Error()}
			return resp
		}
		resp.Result = result
		return resp

	case "validateTemplate":
		var params daemonValidateTemplateParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: fmt.Sprintf("invalid validateTemplate params: %v", err)}
			return resp
		}
		result, err := d.validateTemplate(params)
		if err != nil {
			resp.Error = &rpcError{Code: -32000, Message: err.Error()}
			return resp
		}
		resp.Result = result
		return resp

	case "updateTemplate":
		var params daemonUpdateTemplateParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: fmt.Sprintf("invalid updateTemplate params: %v", err)}
			return resp
		}
		if err := d.updateTemplate(params); err != nil {
			resp.Error = &rpcError{Code: -32000, Message: err.Error()}
			return resp
		}
		resp.Result = map[string]bool{"ok": true}
		return resp

	case "clearTemplate":
		var params daemonClearTemplateParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: fmt.Sprintf("invalid clearTemplate params: %v", err)}
			return resp
		}
		if err := d.clearTemplate(params); err != nil {
			resp.Error = &rpcError{Code: -32000, Message: err.Error()}
			return resp
		}
		resp.Result = map[string]bool{"ok": true}
		return resp

	case "inferExpressionType":
		var params daemonInferExpressionParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: fmt.Sprintf("invalid inferExpressionType params: %v", err)}
			return resp
		}
		result, err := d.inferExpressionType(params)
		if err != nil {
			resp.Error = &rpcError{Code: -32000, Message: err.Error()}
			return resp
		}
		resp.Result = result
		return resp

	case "getHoverInfo":
		var params daemonGetHoverInfoParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: fmt.Sprintf("invalid getHoverInfo params: %v", err)}
			return resp
		}
		result, err := d.getHoverInfo(params)
		if err != nil {
			resp.Error = &rpcError{Code: -32000, Message: err.Error()}
			return resp
		}
		resp.Result = result
		return resp

	case "shutdown":
		resp.Result = map[string]bool{"ok": true}
		return resp

	case "reanalyzeTemplates":
		var params daemonAnalyzeParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: fmt.Sprintf("invalid reanalyzeTemplates params: %v", err)}
			return resp
		}
		result, err := d.reanalyzeTemplates(params)
		if err != nil {
			resp.Error = &rpcError{Code: -32000, Message: err.Error()}
			return resp
		}
		resp.Result = result
		return resp

	default:
		resp.Error = &rpcError{Code: -32601, Message: fmt.Sprintf("unknown method %q", req.Method)}
		return resp
	}
}

func (d *analyzerDaemon) analyze(params daemonAnalyzeParams) (ValidationOutput, error) {
	baseDir := params.Dir
	if params.TemplateBaseDir != "" {
		baseDir = params.TemplateBaseDir
	}

	// Compute fingerprints to detect what changed since last analysis.
	goFP := computeGoFingerprint(params.Dir)
	tmplFP := computeTemplateFingerprint(baseDir, params.TemplateRoot)

	prev := d.state.Load()

	// Fast path: nothing changed at all — return cached output.
	if prev != nil && prev.goFingerprint == goFP && prev.templateFingerprint == tmplFP &&
		prev.dir == params.Dir && prev.templateRoot == params.TemplateRoot &&
		prev.contextFile == params.ContextFile && prev.validate == params.Validate {
		return prev.output, nil
	}

	// Semi-fast path: only templates changed — skip packages.Load, reuse Go analysis.
	if prev != nil && prev.goFingerprint == goFP && prev.analysisResult != nil &&
		prev.dir == params.Dir && prev.contextFile == params.ContextFile {
		return d.buildSnapshotFromResult(prev.analysisResult, params, baseDir, goFP, tmplFP)
	}

	// Cold path: full analysis required.
	result := ast.AnalyzeDir(params.Dir, params.ContextFile, ast.DefaultConfig)
	result.Errors = filterImportErrors(result.Errors)

	return d.buildSnapshotFromResult(&result, params, baseDir, goFP, tmplFP)
}

// buildSnapshotFromResult runs template validation and builds an immutable
// daemon state snapshot from an existing AnalysisResult. This is shared by
// both the full analyze path and the incremental (templates-only) path.
func (d *analyzerDaemon) buildSnapshotFromResult(
	result *ast.AnalysisResult,
	params daemonAnalyzeParams,
	baseDir, goFP, tmplFP string,
) (ValidationOutput, error) {
	validationErrors, namedBlocks, namedBlockErrors := validator.ValidateTemplates(
		result.RenderCalls,
		result.FuncMaps,
		baseDir,
		params.TemplateRoot,
	)

	// Build the render-var index BEFORE Flatten() so field trees are intact.
	renderVarIndex := buildRenderVarIndex(result.RenderCalls)

	// Clone the result before flattening so we can reuse the un-flattened
	// version for future incremental re-validation.
	savedResult := cloneAnalysisResult(result)

	result.Flatten()

	output := ValidationOutput{
		RenderCalls:      result.RenderCalls,
		FuncMaps:         result.FuncMaps,
		Errors:           result.Errors,
		NamedBlocks:      namedBlocks,
		NamedBlockErrors: namedBlockErrors,
		Types:            result.Types,
	}
	if params.Validate {
		output.ValidationErrors = validationErrors
		output.NamedBlocks = namedBlocks
	}

	// Build immutable snapshot — no cloning needed by readers.
	snap := &daemonState{
		dir:                  params.Dir,
		baseDir:              baseDir,
		templateRoot:         params.TemplateRoot,
		contextFile:          params.ContextFile,
		validate:             params.Validate,
		output:               output,
		renderVarsByTemplate: renderVarIndex,
		funcMaps:             validator.BuildFuncMapRegistry(result.FuncMaps),
		typeRegistry:         result.Types,
		namedBlocks:          namedBlocks,
		partialTargets:       validator.FindPartialTargets(baseDir, params.TemplateRoot),
		goFingerprint:        goFP,
		templateFingerprint:  tmplFP,
		analysisResult:       savedResult,
	}

	// Atomic swap: readers instantly see the new state without waiting.
	d.state.Store(snap)

	// Preserve existing overlays (don't reset on re-analyze).
	if d.templateOverlays == nil {
		d.overlayMu.Lock()
		d.templateOverlays = make(map[string]string)
		d.overlayMu.Unlock()
	}

	return output, nil
}

// reanalyzeTemplates re-runs only the template validation step using the
// existing Go analysis result. This is much faster than a full analyze when
// only template files have changed.
func (d *analyzerDaemon) reanalyzeTemplates(params daemonAnalyzeParams) (ValidationOutput, error) {
	prev := d.state.Load()
	if prev == nil || prev.analysisResult == nil {
		// No previous analysis — fall back to full analyze.
		return d.analyze(params)
	}

	baseDir := params.Dir
	if params.TemplateBaseDir != "" {
		baseDir = params.TemplateBaseDir
	}

	goFP := prev.goFingerprint
	tmplFP := computeTemplateFingerprint(baseDir, params.TemplateRoot)

	return d.buildSnapshotFromResult(prev.analysisResult, params, baseDir, goFP, tmplFP)
}

// cloneAnalysisResult creates a shallow copy of an AnalysisResult so the
// original's RenderCalls and FuncMaps are preserved before Flatten() mutates them.
func cloneAnalysisResult(r *ast.AnalysisResult) *ast.AnalysisResult {
	clone := &ast.AnalysisResult{
		Errors: r.Errors,
		Types:  r.Types,
	}
	// Deep-copy render calls since Flatten() strips field trees in-place.
	clone.RenderCalls = make([]ast.RenderCall, len(r.RenderCalls))
	for i, rc := range r.RenderCalls {
		clone.RenderCalls[i] = rc
		clone.RenderCalls[i].Vars = make([]ast.TemplateVar, len(rc.Vars))
		copy(clone.RenderCalls[i].Vars, rc.Vars)
	}
	clone.FuncMaps = make([]ast.FuncMapInfo, len(r.FuncMaps))
	copy(clone.FuncMaps, r.FuncMaps)
	return clone
}

// computeGoFingerprint builds a hash from all .go file paths and their
// modification times under dir (excluding vendor, node_modules, testdata).
func computeGoFingerprint(dir string) string {
	h := sha256.New()
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == "vendor" || name == "node_modules" || name == "testdata" ||
				strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") {
			info, err := d.Info()
			if err != nil {
				return nil
			}
			fmt.Fprintf(h, "%s:%d\n", path, info.ModTime().UnixNano())
		}
		return nil
	})
	// Also include go.sum if present.
	if info, err := os.Stat(filepath.Join(dir, "go.sum")); err == nil {
		fmt.Fprintf(h, "go.sum:%d\n", info.ModTime().UnixNano())
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// computeTemplateFingerprint builds a hash from all template file paths and
// their modification times under baseDir/templateRoot.
func computeTemplateFingerprint(baseDir, templateRoot string) string {
	root := filepath.Join(baseDir, templateRoot)
	h := sha256.New()
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".html") || strings.HasSuffix(path, ".tmpl") {
			info, err := d.Info()
			if err != nil {
				return nil
			}
			fmt.Fprintf(h, "%s:%d\n", path, info.ModTime().UnixNano())
		}
		return nil
	})
	return fmt.Sprintf("%x", h.Sum(nil))
}

func (d *analyzerDaemon) validateTemplate(params daemonValidateTemplateParams) (result daemonValidateTemplateResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("validateTemplate panic: %v", recovered)
		}
	}()

	// OPTIMISATION: Load the immutable snapshot atomically — zero allocation,
	// no mutex acquisition for the read-heavy path.
	snap := d.state.Load()
	if snap == nil {
		return daemonValidateTemplateResult{}, fmt.Errorf("daemon not initialized")
	}

	if !snap.validate {
		return daemonValidateTemplateResult{HasContext: false}, nil
	}

	absPath, err := filepath.Abs(params.AbsolutePath)
	if err != nil {
		return daemonValidateTemplateResult{}, err
	}

	templateBase := filepath.Join(snap.baseDir, snap.templateRoot)
	rel, err := filepath.Rel(templateBase, absPath)
	if err != nil {
		rel = absPath
	}
	rel = filepath.ToSlash(rel)

	// Load overlays under read lock (cheap: just a map lookup).
	d.overlayMu.RLock()
	overlays := cloneTemplateOverlays(d.templateOverlays)
	d.overlayMu.RUnlock()

	overlays[absPath] = params.Content

	// Build a per-request registry copy only when overlays change the shape.
	// For the common case (no overlays beyond the current file), we reuse the
	// shared namedBlocks snapshot directly — only clone when we must mutate.
	registry := snap.namedBlocks
	if len(overlays) > 0 {
		registry = cloneRegistry(snap.namedBlocks)
		applyTemplateOverlays(registry, overlays, snap.baseDir, snap.templateRoot)
	}

	var errors []validator.ValidationResult
	hasContext := false

	if _, vars, ok := findRenderVarsForTemplate(snap.renderVarsByTemplate, absPath, snap.baseDir, snap.templateRoot); ok {
		hasContext = true
		errors = append(errors, validator.ValidateTemplateFileStr(
			params.Content,
			vars,
			rel,
			snap.baseDir,
			snap.templateRoot,
			registry,
			snap.funcMaps,
		)...)
	}

	for _, entry := range registryEntriesForFile(registry, absPath) {
		if entry.Name == entry.TemplatePath {
			continue
		}
		if snap.partialTargets[entry.Name] {
			continue
		}
		vars, ok := snap.renderVarsByTemplate[entry.Name]
		if !ok {
			continue
		}
		hasContext = true
		errors = append(errors, validator.ValidateNamedBlockContent(
			entry.Content,
			vars,
			entry.TemplatePath,
			snap.baseDir,
			snap.templateRoot,
			entry.Line,
			registry,
			snap.funcMaps,
		)...)
	}

	return daemonValidateTemplateResult{
		ValidationErrors: dedupeValidationErrors(errors),
		HasContext:       hasContext,
	}, nil
}

func (d *analyzerDaemon) updateTemplate(params daemonUpdateTemplateParams) error {
	absPath, err := filepath.Abs(params.AbsolutePath)
	if err != nil {
		return err
	}
	d.overlayMu.Lock()
	if d.templateOverlays == nil {
		d.templateOverlays = make(map[string]string)
	}
	d.templateOverlays[absPath] = params.Content
	d.overlayMu.Unlock()
	return nil
}

func (d *analyzerDaemon) clearTemplate(params daemonClearTemplateParams) error {
	absPath, err := filepath.Abs(params.AbsolutePath)
	if err != nil {
		return err
	}
	d.overlayMu.Lock()
	delete(d.templateOverlays, absPath)
	d.overlayMu.Unlock()
	return nil
}

func (d *analyzerDaemon) inferExpressionType(params daemonInferExpressionParams) (*validator.ExpressionTypeResult, error) {
	snap := d.state.Load()
	if snap == nil {
		return nil, fmt.Errorf("daemon not initialized")
	}

	// Read-only: no cloning needed.
	return validator.InferExpressionType(
		params.Expression,
		params.Vars,
		params.ScopeStack,
		params.BlockLocals,
		snap.funcMaps,
		snap.typeRegistry,
	), nil
}

func (d *analyzerDaemon) getHoverInfo(params daemonGetHoverInfoParams) (*validator.HoverResult, error) {
	snap := d.state.Load()
	if snap == nil {
		return nil, fmt.Errorf("daemon not initialized")
	}

	absPath, err := filepath.Abs(params.AbsolutePath)
	if err != nil {
		return nil, err
	}

	// Load overlays under read lock.
	d.overlayMu.RLock()
	overlays := cloneTemplateOverlays(d.templateOverlays)
	d.overlayMu.RUnlock()

	content := params.Content
	if content == "" {
		if overlay, ok := overlays[absPath]; ok {
			content = overlay
		}
	}
	if content == "" {
		return nil, fmt.Errorf("no content for %s", absPath)
	}

	templateBase := filepath.Join(snap.baseDir, snap.templateRoot)
	rel, err := filepath.Rel(templateBase, absPath)
	if err != nil {
		rel = absPath
	}
	rel = filepath.ToSlash(rel)

	registry := snap.namedBlocks
	if len(overlays) > 0 {
		registry = cloneRegistry(snap.namedBlocks)
		applyTemplateOverlays(registry, overlays, snap.baseDir, snap.templateRoot)
	}

	_, vars, ok := findRenderVarsForTemplate(snap.renderVarsByTemplate, absPath, snap.baseDir, snap.templateRoot)
	if !ok {
		return nil, nil
	}

	varMap := make(map[string]ast.TemplateVar, len(vars))
	for _, v := range vars {
		varMap[v.Name] = v
	}

	result := validator.GetHoverResult(
		content, varMap, rel, snap.baseDir, snap.templateRoot,
		0,
		params.Line, params.Col,
		registry, snap.funcMaps, snap.typeRegistry,
	)
	return result, nil
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func findRenderVarsForTemplate(
	renderVarsByTemplate map[string][]ast.TemplateVar,
	absPath, baseDir, templateRoot string,
) (string, []ast.TemplateVar, bool) {
	templateBase := filepath.Join(baseDir, templateRoot)
	rel := normalizeTemplateKey(absPath)
	if relPath, err := filepath.Rel(templateBase, absPath); err == nil {
		rel = normalizeTemplateKey(relPath)
	}

	if vars, ok := renderVarsByTemplate[rel]; ok {
		return rel, vars, true
	}

	for key, vars := range renderVarsByTemplate {
		normalizedKey := normalizeTemplateKey(key)
		candidateAbs := filepath.Join(templateBase, normalizedKey)
		if normalizePath(candidateAbs) == normalizePath(absPath) {
			return key, vars, true
		}
		if strings.HasSuffix(rel, normalizedKey) || strings.HasSuffix(normalizedKey, rel) {
			return key, vars, true
		}
	}

	baseName := filepath.Base(absPath)
	for key, vars := range renderVarsByTemplate {
		if filepath.Base(normalizeTemplateKey(key)) == baseName {
			return key, vars, true
		}
	}

	return "", nil, false
}

// buildRenderVarIndex creates a template-name → merged TemplateVar list from
// all render calls. When multiple render calls target the same template, their
// variable sets are unioned so downstream validation sees the broadest context.
func buildRenderVarIndex(renderCalls []ast.RenderCall) map[string][]ast.TemplateVar {
	idx := make(map[string][]ast.TemplateVar, len(renderCalls))
	seen := make(map[string]map[string]bool, len(renderCalls))

	for _, rc := range renderCalls {
		if _, ok := idx[rc.Template]; !ok {
			idx[rc.Template] = nil
			seen[rc.Template] = make(map[string]bool)
		}
		for _, v := range rc.Vars {
			if !seen[rc.Template][v.Name] {
				seen[rc.Template][v.Name] = true
				idx[rc.Template] = append(idx[rc.Template], v)
			}
		}
	}

	return idx
}

func cloneRegistry(in map[string][]validator.NamedBlockEntry) map[string][]validator.NamedBlockEntry {
	out := make(map[string][]validator.NamedBlockEntry, len(in))
	for key, entries := range in {
		out[key] = append([]validator.NamedBlockEntry(nil), entries...)
	}
	return out
}

func cloneTemplateOverlays(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func applyTemplateOverlays(registry map[string][]validator.NamedBlockEntry, overlays map[string]string, baseDir, templateRoot string) {
	templateBase := filepath.Join(baseDir, templateRoot)
	for absolutePath, content := range overlays {
		if rel, err := filepath.Rel(templateBase, absolutePath); err == nil {
			replaceRegistryEntriesForFile(registry, absolutePath, content, filepath.ToSlash(rel))
			continue
		}
		replaceRegistryEntriesForFile(registry, absolutePath, content, absolutePath)
	}
}

func replaceRegistryEntriesForFile(registry map[string][]validator.NamedBlockEntry, absolutePath, content, templatePath string) {
	normalizedPath := normalizePath(absolutePath)
	for name, entries := range registry {
		filtered := entries[:0]
		for _, entry := range entries {
			if normalizePath(entry.AbsolutePath) != normalizedPath {
				filtered = append(filtered, entry)
			}
		}
		if len(filtered) == 0 {
			delete(registry, name)
			continue
		}
		registry[name] = filtered
	}

	validator.ExtractNamedTemplatesFromContent(content, absolutePath, templatePath, registry)

	registry[templatePath] = append(registry[templatePath], validator.NamedBlockEntry{
		Name:         templatePath,
		AbsolutePath: absolutePath,
		TemplatePath: templatePath,
		Line:         1,
		Col:          1,
		Content:      content,
	})
}

func registryEntriesForFile(registry map[string][]validator.NamedBlockEntry, absolutePath string) []validator.NamedBlockEntry {
	normalizedPath := normalizePath(absolutePath)
	entries := make([]validator.NamedBlockEntry, 0)
	for _, blockEntries := range registry {
		for _, entry := range blockEntries {
			if normalizePath(entry.AbsolutePath) == normalizedPath {
				entries = append(entries, entry)
			}
		}
	}
	return entries
}

// dedupKey is a struct used to identify unique validation errors for deduplication purposes.
// Since all fields are comparable, we can use this as a key in a map without memory allocation.
type dedupKey struct {
	Template string
	Line     int
	Column   int
	Variable string
	Message  string
}

func dedupeValidationErrors(in []validator.ValidationResult) []validator.ValidationResult {
	seen := make(map[dedupKey]bool, len(in))
	out := make([]validator.ValidationResult, 0, len(in))
	for _, err := range in {
		key := dedupKey{err.Template, err.Line, err.Column, err.Variable, err.Message}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, err)
	}
	return out
}

func normalizePath(value string) string {
	return filepath.Clean(strings.ToLower(value))
}

func normalizeTemplateKey(value string) string {
	cleaned := filepath.ToSlash(filepath.Clean(value))
	return strings.TrimPrefix(cleaned, "./")
}
