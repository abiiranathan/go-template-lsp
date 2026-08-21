package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/abiiranathan/go-template-lsp/gotpl-analyzer/ast"
	"github.com/abiiranathan/go-template-lsp/gotpl-analyzer/validator"
)

// rpcRequest represents an incoming JSON-RPC 2.0 request from the extension client.
type rpcRequest struct {
	// JSONRPC is the protocol version string, always "2.0".
	JSONRPC string `json:"jsonrpc"`
	// ID is the unique request identifier used to correlate responses.
	ID int64 `json:"id"`
	// Method is the RPC method name to invoke (e.g. "analyze", "validateTemplate").
	Method string `json:"method"`
	// Params is the raw JSON payload of method-specific parameters.
	Params json.RawMessage `json:"params,omitempty"`
}

// rpcResponse represents an outgoing JSON-RPC 2.0 response sent back to the extension client.
type rpcResponse struct {
	// JSONRPC is the protocol version string, always "2.0".
	JSONRPC string `json:"jsonrpc"`
	// ID is the request identifier this response corresponds to.
	ID int64 `json:"id"`
	// Result is the successful response payload; nil when Error is set.
	Result any `json:"result,omitempty"`
	// Error is the error object; nil on success.
	Error *rpcError `json:"error,omitempty"`
}

// rpcError represents a JSON-RPC 2.0 error object with a numeric code and human-readable message.
type rpcError struct {
	// Code is a numeric error code (e.g. -32700 for parse error, -32601 for method not found).
	Code int `json:"code"`
	// Message is a human-readable description of the error.
	Message string `json:"message"`
}

// daemonAnalyzeParams holds the parameters for the "analyze" RPC method.
// Dir is the Go module root, TemplateRoot is the relative path to templates,
// and ContextFile is an optional rex-analyzer.json providing extra context.
type daemonAnalyzeParams struct {
	// Dir is the absolute path to the Go module root directory.
	Dir string `json:"dir"`

	// TemplateRoot is the relative path from the base directory to the template folder.
	TemplateRoot string `json:"templateRoot"`

	// TemplateBaseDir overrides Dir as the base for resolving TemplateRoot. If empty, Dir is used.
	TemplateBaseDir string `json:"templateBaseDir"`

	// ContextFile is the optional path to a rex-analyzer.json file providing additional context.
	ContextFile string `json:"contextFile"`

	// Validate enables template validation against Go types when true.
	Validate bool `json:"validate"`

	// RenderFunctionNames is a slice of function or method names that render templates
	RenderFunctionNames []string `json:"renderFunctionNames,omitempty"`

	// List of method names used to set context variables (e.g. c.Set, c.Locals).
	SetFunctionNames []string `json:"setFunctionNames,omitempty"`

	// List of Go type names representing the web context receiver.
	ContextTypeNames []string `json:"contextTypeNames,omitempty"`
}

// daemonValidateTemplateParams holds the parameters for the "validateTemplate" RPC method.
// AbsolutePath is the on-disk path of the template and Content is the current editor buffer.
type daemonValidateTemplateParams struct {
	// AbsolutePath is the absolute on-disk path of the template file.
	AbsolutePath string `json:"absolutePath"`
	// Content is the current editor buffer content for the template.
	Content string `json:"content"`
}

// daemonUpdateTemplateParams holds the parameters for the "updateTemplate" RPC method.
// It stores an in-memory overlay of unsaved template content for the given file.
type daemonUpdateTemplateParams struct {
	// AbsolutePath is the absolute on-disk path of the template file to overlay.
	AbsolutePath string `json:"absolutePath"`

	// Content is the unsaved editor buffer content to store as an overlay.
	Content string `json:"content"`
}

// daemonClearTemplateParams holds the parameters for the "clearTemplate" RPC method.
// It removes the in-memory overlay for the given file, reverting to the on-disk version.
type daemonClearTemplateParams struct {
	// AbsolutePath is the absolute path of the template whose overlay should be removed.
	AbsolutePath string `json:"absolutePath"`
}

// daemonInferExpressionParams holds the parameters for the "inferExpressionType" RPC method.
// The extension sends the expression text along with the current scope context so the
// daemon can resolve the expression's type without re-parsing the entire template.
type daemonInferExpressionParams struct {
	// Expression is the template expression to resolve (e.g. ".Name", ".Items | len").
	Expression string `json:"expression"`

	// Vars is the set of template variables available at the expression's scope.
	Vars map[string]ast.TemplateVar `json:"vars"`

	// ScopeStack is the nesting of block scopes (range, with, if) surrounding the expression.
	ScopeStack []validator.ScopeType `json:"scopeStack"`

	// BlockLocals contains variables declared by {{$x := ...}} within the current block.
	BlockLocals map[string]ast.TemplateVar `json:"blockLocals"`
}

// daemonGetHoverInfoParams holds the parameters for the "getHoverInfo" RPC method.
// The extension sends the cursor position (1-based line and column) along with the
// current buffer content so the daemon can return type and documentation information.
type daemonGetHoverInfoParams struct {
	// AbsolutePath is the absolute on-disk path of the template file.
	AbsolutePath string `json:"absolutePath"`

	// Line is the 1-based line number of the cursor position.
	Line int `json:"line"`

	// Col is the 1-based column number of the cursor position.
	Col int `json:"col"`

	// Content is the current editor buffer content for the template.
	Content string `json:"content"`
}

// daemonValidateTemplateResult is the response payload for the "validateTemplate" RPC method.
// ValidationErrors contains any issues found, and HasContext indicates whether render-call
// variable context was available for the template (if false, validation was skipped).
type daemonValidateTemplateResult struct {
	// ValidationErrors is the list of validation issues found in the template.
	ValidationErrors []validator.ValidationResult `json:"validationErrors"`

	// HasContext is true when render-call variable context was found for the template.
	HasContext bool `json:"hasContext"`
}

// daemonState is the immutable snapshot of analysis results shared by all
// concurrent read-only operations (validateTemplate, inferExpressionType,
// getHoverInfo).  The pointer is replaced atomically on each analyze call so
// readers always see a consistent snapshot without acquiring a write lock.
type daemonState struct {
	// dir is the Go module root directory used for analysis.
	dir string

	// baseDir is the resolved base directory for template lookup (may differ from dir).
	baseDir string

	// templateRoot is the relative path from baseDir to the template folder.
	templateRoot string

	// contextFile is the path to the optional rex-analyzer.json context file.
	contextFile string

	// validate indicates whether template validation was enabled for this snapshot.
	validate bool

	// output is the complete validation output returned by the last analyze call.
	output ValidationOutput

	// renderVarsByTemplate maps template names to their merged set of render-call variables.
	renderVarsByTemplate map[string][]ast.TemplateVar

	// funcMaps is the registry of all template function maps discovered in the Go source.
	funcMaps validator.FuncMapRegistry

	// typeRegistry maps fully-qualified Go type names to their field information.
	typeRegistry map[string][]ast.FieldInfo

	// namedBlocks maps block names to their definition entries across all template files.
	namedBlocks map[string][]validator.NamedBlockEntry

	// partialTargets is a set of template names that are invoked as partials (via {{template}}).
	partialTargets map[string]bool

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

// analyzerDaemon is the main daemon server that processes JSON-RPC requests over
// stdin/stdout. It maintains an immutable analysis snapshot (swapped atomically)
// and a mutable template overlay map (protected by a RWMutex).
type analyzerDaemon struct {
	// state is replaced atomically on analyze; read-only handlers load it with
	// atomic.Pointer.Load() which does not block.
	state atomic.Pointer[daemonState]

	// templateOverlays is the only field that mutates after analyze completes
	// (via updateTemplate / clearTemplate).  Protected by its own fine-grained
	// RWMutex instead of the coarse daemon-wide lock.
	// overlayMu protects templateOverlays for concurrent read/write access.
	overlayMu sync.RWMutex

	// templateOverlays maps absolute template file paths to their unsaved editor buffer content.
	templateOverlays map[string]string

	// Serializes analyze requests to avoid multiple concurrent packages.Load calls
	analyzeMu sync.Mutex
}

var (
	logFileOnce sync.Once
	logFile     *os.File
	logMu       sync.Mutex
)

func getLogWriter() *os.File {
	logFileOnce.Do(func() {
		f, err := os.OpenFile(filepath.Join(os.TempDir(), "gotpl-daemon.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err == nil {
			logFile = f
		}
	})
	return logFile
}

// debugLog writes to a log file in the temp directory.
// Since stdout is used for RPC, this is the only way to see what's happening.
func debugLog(format string, v ...any) {
	f := getLogWriter()
	if f == nil {
		return
	}
	logMu.Lock()
	defer logMu.Unlock()
	fmt.Fprintf(f, "[DAEMON] "+format+"\n", v...)
}

// runDaemon starts a JSON-RPC 2.0 server over stdin/stdout that serves
// analysis, validation, hover, and type-inference requests. The daemon
// maintains an immutable snapshot of analysis results that is atomically
// swapped on each analyze call, enabling lock-free reads for concurrent
// read-only operations.
func runDaemon(stdin io.Reader, stdout io.Writer) error {
	// Check if 'go' is available in current PATH.
	if _, err := exec.LookPath("go"); err != nil {
		debugLog("WARNING: 'go' not found in PATH. Attempting to repair PATH...")
		// Try adding standard Go binary paths
		newPath := os.Getenv("PATH") + ":/usr/local/go/bin:" + filepath.Join(os.Getenv("HOME"), "go/bin")
		os.Setenv("PATH", newPath)
		if _, err2 := exec.LookPath("go"); err2 == nil {
			debugLog("Successfully repaired PATH. 'go' is now available.")
		} else {
			debugLog("CRITICAL: Failed to find 'go' even after repair. Analysis WILL fail.")
		}
	}

	server := &analyzerDaemon{
		templateOverlays: make(map[string]string),
	}
	reader := bufio.NewReader(stdin)
	writer := bufio.NewWriter(stdout)

	debugLog("Daemon started. Monitoring stdin for RPC requests.")

	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				debugLog("Stdin closed (EOF). Shutting down.")
				return nil
			}
			debugLog("Error reading stdin: %v", err)
			return err
		}

		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			debugLog("Parse error on request: %v", err)
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
			debugLog("Error writing response: %v", err)
			return err
		}

		if req.Method == "shutdown" {
			debugLog("Shutdown requested.")
			return nil
		}
	}
}

// writeResponse marshals an rpcResponse as JSON and writes it as a single
// newline-terminated line to the buffered writer, then flushes immediately.
func writeResponse(writer *bufio.Writer, resp rpcResponse) error {
	if err := json.NewEncoder(writer).Encode(resp); err != nil {
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

func (p daemonAnalyzeParams) toAnalysisConfig() *ast.AnalysisConfig {
	cfg := ast.DefaultConfig
	if len(p.RenderFunctionNames) > 0 {
		cfg.RenderFunctionNames = p.RenderFunctionNames
	}
	if len(p.SetFunctionNames) > 0 {
		cfg.SetFunctionNames = p.SetFunctionNames
	}
	if len(p.ContextTypeNames) > 0 {
		cfg.ContextTypeNames = p.ContextTypeNames
	}
	return &cfg
}

// analyze performs a full or incremental project analysis. It uses fingerprinting
// to detect changes: if nothing changed, the cached output is returned immediately;
// if only templates changed, Go analysis is reused; otherwise a full packages.Load
// is performed.
func (d *analyzerDaemon) analyze(params daemonAnalyzeParams) (ValidationOutput, error) {
	d.analyzeMu.Lock()
	defer d.analyzeMu.Unlock()

	debugLog("Starting analysis for dir: %s", params.Dir)

	// Resolve absolute paths
	absDir, err := filepath.Abs(params.Dir)
	if err != nil {
		return ValidationOutput{}, fmt.Errorf("invalid analysis directory: %v", err)
	}
	params.Dir = absDir

	baseDir := params.Dir
	if params.TemplateBaseDir != "" {
		absBase, err := filepath.Abs(params.TemplateBaseDir)
		if err != nil {
			return ValidationOutput{}, fmt.Errorf("invalid template base directory: %v", err)
		}
		baseDir = absBase
	}

	// Compute fingerprints.
	goFP, err := computeGoFingerprint(params.Dir)
	if err != nil {
		debugLog("Error fingerprinting Go files: %v", err)
	}
	tmplFP, err := computeTemplateFingerprint(baseDir, params.TemplateRoot)
	if err != nil {
		debugLog("Error fingerprinting templates: %v", err)
	}

	// CACHE REMOVED for now to ensure reliability.

	// Cold path: Full Go source analysis
	result := ast.AnalyzeDir(params.Dir, params.ContextFile, params.toAnalysisConfig())

	if len(result.Errors) > 0 {
		debugLog("Go analysis returned %d raw errors", len(result.Errors))
		for _, e := range result.Errors {
			debugLog("Analysis error: %s", e)
		}
	}

	result.Errors = filterImportErrors(result.Errors)
	debugLog("Found %d render calls.", len(result.RenderCalls))

	return d.buildSnapshotFromResult(&result, params, baseDir, goFP, tmplFP)
}

// buildSnapshotFromResult runs template validation and builds an immutable
// daemon state snapshot from an existing AnalysisResult. This is shared by
// both the full analyze path and the incremental (templates-only) path.
// buildSnapshotFromResult runs template validation and builds an immutable
// daemon state snapshot from an existing AnalysisResult.
func (d *analyzerDaemon) buildSnapshotFromResult(
	rawResult *ast.AnalysisResult,
	params daemonAnalyzeParams,
	baseDir, goFP, tmplFP string,
) (ValidationOutput, error) {
	// 1. Run template validation. The type registry is NOT built here —
	// Flatten() below populates it just before stripping the inline field
	// trees it is derived from, so an explicit earlier build would be a
	// redundant full tree walk per analyze cycle.
	// The template store is loaded once here and shared with validation;
	// loading it inside ValidateTemplates as well would double the disk walk
	// and transiently retain two copies of every template's content.
	store := validator.LoadTemplateStore(baseDir, params.TemplateRoot)
	validationErrors, namedBlocks, namedBlockErrors := validator.ValidateTemplatesWithStore(
		rawResult.RenderCalls,
		rawResult.FuncMaps,
		baseDir,
		params.TemplateRoot,
		store,
	)

	funcMapReg := validator.BuildFuncMapRegistry(rawResult.FuncMaps)

	// 2. Build render-var index with partial & named-block propagation
	renderVarIndex := validator.BuildPropagatedRenderVarIndex(
		rawResult.RenderCalls, namedBlocks, baseDir, params.TemplateRoot, funcMapReg, store,
	)

	// 3. Flatten rawResult in-place (strips inline field trees, keeping only Types registry)
	rawResult.Flatten()

	// 4. Prepare Output RenderCalls without deep-copying field trees
	outputRenderCalls := make([]ast.RenderCall, len(rawResult.RenderCalls))
	copy(outputRenderCalls, rawResult.RenderCalls)

	existingCalls := make(map[string]bool, len(outputRenderCalls))
	for i := range outputRenderCalls {
		rc := &outputRenderCalls[i]
		existingCalls[rc.Template] = true
		if propagated, ok := renderVarIndex[rc.Template]; ok && len(propagated) > 0 {
			rc.Vars = shallowMergeVars(rc.Vars, propagated)
			// Propagated variables are resolved through scope walking and may
			// carry inline field trees; strip them so the response matches the
			// ValidationOutput contract (consumers resolve types via Types).
			// Only the copied structs are mutated — the snapshot's
			// renderVarsByTemplate keeps its trees for hover/validation.
			stripInlineTrees(rc.Vars)
		}
	}

	for tplName, vars := range renderVarIndex {
		if !existingCalls[tplName] && len(vars) > 0 {
			syntheticVars := shallowCopyVars(vars)
			stripInlineTrees(syntheticVars)
			outputRenderCalls = append(outputRenderCalls, ast.RenderCall{
				File:     "template-call",
				Line:     1,
				Template: tplName,
				Vars:     syntheticVars,
			})
		}
	}

	// Filter out synthetic whole-file entries from output namedBlocks to keep JSON small
	cleanNamedBlocks := make(map[string][]validator.NamedBlockEntry, len(namedBlocks))
	for name, entries := range namedBlocks {
		if validator.IsFileBasedPartial(name) {
			continue
		}
		cleanNamedBlocks[name] = entries
	}

	output := ValidationOutput{
		RenderCalls:      outputRenderCalls,
		FuncMaps:         rawResult.FuncMaps,
		Errors:           rawResult.Errors,
		NamedBlocks:      cleanNamedBlocks,
		NamedBlockErrors: namedBlockErrors,
		Types:            rawResult.Types,
	}
	if params.Validate {
		output.ValidationErrors = validationErrors
	}

	snap := &daemonState{
		dir:                  params.Dir,
		baseDir:              baseDir,
		templateRoot:         params.TemplateRoot,
		contextFile:          params.ContextFile,
		validate:             params.Validate,
		output:               output,
		renderVarsByTemplate: renderVarIndex,
		funcMaps:             funcMapReg,
		typeRegistry:         rawResult.Types,
		namedBlocks:          namedBlocks,
		partialTargets:       validator.FindPartialTargets(baseDir, params.TemplateRoot),
		goFingerprint:        goFP,
		templateFingerprint:  tmplFP,
		analysisResult:       rawResult,
	}

	d.state.Store(snap)

	return output, nil
}

// shallowCopyVars copies variable slices without recursive field tree cloning.
func shallowCopyVars(vars []ast.TemplateVar) []ast.TemplateVar {
	if len(vars) == 0 {
		return nil
	}
	out := make([]ast.TemplateVar, len(vars))
	copy(out, vars)
	return out
}

// stripInlineTrees clears the top-level field tree of each variable. Because
// nested FieldInfo trees are only reachable through this slice, a single nil
// assignment drops the whole tree without walking it.
func stripInlineTrees(vars []ast.TemplateVar) {
	for i := range vars {
		vars[i].Fields = nil
	}
}

// shallowMergeVars merges two variable slices by name with zero recursion overhead.
func shallowMergeVars(a, b []ast.TemplateVar) []ast.TemplateVar {
	seen := make(map[string]struct{}, len(a)+len(b))
	res := make([]ast.TemplateVar, 0, len(a)+len(b))
	for _, v := range a {
		if _, exists := seen[v.Name]; !exists {
			seen[v.Name] = struct{}{}
			res = append(res, v)
		}
	}
	for _, v := range b {
		if _, exists := seen[v.Name]; !exists {
			seen[v.Name] = struct{}{}
			res = append(res, v)
		}
	}
	return res
}

func (d *analyzerDaemon) reanalyzeTemplates(params daemonAnalyzeParams) (ValidationOutput, error) {
	// Always perform full analyze while cache is disabled to ensure correct template resolution.
	return d.analyze(params)
}

// computeGoFingerprint creates an mtime-based fingerprint of all Go source files.
func computeGoFingerprint(dir string) (string, error) {
	h := sha256.New()
	var numBuf [32]byte
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == "vendor" || name == "node_modules" || name == "testdata" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") {
			info, err := d.Info()
			if err != nil {
				return err
			}
			io.WriteString(h, path)
			h.Write([]byte{':'})
			h.Write(strconv.AppendInt(numBuf[:0], info.ModTime().UnixNano(), 10))
			h.Write([]byte{'\n'})
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(filepath.Join(dir, "go.sum")); err == nil {
		io.WriteString(h, "go.sum:")
		h.Write(strconv.AppendInt(numBuf[:0], info.ModTime().UnixNano(), 10))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// computeTemplateFingerprint creates an mtime-based fingerprint of template files.
func computeTemplateFingerprint(baseDir, templateRoot string) (string, error) {
	root := filepath.Join(baseDir, templateRoot)
	h := sha256.New()
	var numBuf [32]byte
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".html") || strings.HasSuffix(path, ".tmpl") {
			info, err := d.Info()
			if err != nil {
				return err
			}
			io.WriteString(h, path)
			h.Write([]byte{':'})
			h.Write(strconv.AppendInt(numBuf[:0], info.ModTime().UnixNano(), 10))
			h.Write([]byte{'\n'})
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
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
	var overlays map[string]string
	if len(d.templateOverlays) > 0 {
		overlays = cloneTemplateOverlays(d.templateOverlays)
	} else {
		overlays = make(map[string]string, 1)
	}
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
		if entry.Name == entry.TemplatePath || snap.partialTargets[entry.Name] {
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

// updateTemplate stores an in-memory overlay of the template content so that
// subsequent validateTemplate and getHoverInfo calls use the unsaved buffer
// instead of the on-disk file.
func (d *analyzerDaemon) updateTemplate(params daemonUpdateTemplateParams) error {
	absPath, err := filepath.Abs(params.AbsolutePath)
	if err != nil {
		return err
	}
	d.overlayMu.Lock()
	if d.templateOverlays == nil {
		d.templateOverlays = make(map[string]string, 1)
	}
	d.templateOverlays[absPath] = params.Content
	d.overlayMu.Unlock()
	return nil
}

// clearTemplate removes the in-memory overlay for a template file, causing
// future operations to read from disk.
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

// inferExpressionType resolves the Go type of a template expression (e.g. ".Name",
// ".Items | len") using the current analysis snapshot's type registry and func maps.
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

// getHoverInfo returns type and documentation information for the symbol at the
// given cursor position in a template file, used to power VS Code hover tooltips.
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
	var overlays map[string]string
	if len(d.templateOverlays) > 0 {
		overlays = cloneTemplateOverlays(d.templateOverlays)
	}
	d.overlayMu.RUnlock()

	content := params.Content
	if content == "" && len(overlays) > 0 {
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

	_, vars, ok := findRenderVarsForTemplateWithRegistry(snap.renderVarsByTemplate, absPath, snap.baseDir, snap.templateRoot, registry)
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

func findRenderVarsForTemplateWithRegistry(
	renderVarsByTemplate map[string][]ast.TemplateVar,
	absPath, baseDir, templateRoot string,
	registry map[string][]validator.NamedBlockEntry,
) (string, []ast.TemplateVar, bool) {
	if key, vars, ok := findRenderVarsForTemplate(renderVarsByTemplate, absPath, baseDir, templateRoot); ok {
		return key, vars, true
	}

	normalizedAbs := normalizePath(absPath)
	for blockName, entries := range registry {
		for _, entry := range entries {
			if normalizePath(entry.AbsolutePath) == normalizedAbs {
				if vars, ok := renderVarsByTemplate[blockName]; ok && len(vars) > 0 {
					return blockName, vars, true
				}
			}
		}
	}

	return "", nil, false
}

// findRenderVarsForTemplate looks up the template variables associated with a
// template file by trying several key normalization strategies (relative path,
// suffix match, basename match). Returns the matched key, variables, and whether
// a match was found.
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

	normalizedAbs := normalizePath(absPath)
	for key, vars := range renderVarsByTemplate {
		normalizedKey := normalizeTemplateKey(key)
		candidateAbs := filepath.Join(templateBase, normalizedKey)
		if normalizePath(candidateAbs) == normalizedAbs {
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

// cloneRegistry creates a shallow copy of the named block registry so that
// overlay mutations do not affect the shared immutable snapshot.
func cloneRegistry(in map[string][]validator.NamedBlockEntry) map[string][]validator.NamedBlockEntry {
	out := make(map[string][]validator.NamedBlockEntry, len(in))
	for key, entries := range in {
		out[key] = append([]validator.NamedBlockEntry(nil), entries...)
	}
	return out
}

// cloneTemplateOverlays creates a shallow copy of the template overlay map
// so that reads under RLock can safely iterate without holding the lock.
func cloneTemplateOverlays(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	maps.Copy(out, in)
	return out
}

// applyTemplateOverlays updates the named block registry with in-memory template
// content from overlays, replacing any existing entries for the same file paths.
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

// registryEntriesForFile returns all named block entries in the registry whose
// absolute path matches the given file, used to find blocks defined in a template.
func registryEntriesForFile(registry map[string][]validator.NamedBlockEntry, absolutePath string) []validator.NamedBlockEntry {
	normalizedPath := normalizePath(absolutePath)
	var entries []validator.NamedBlockEntry
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
	// Template is the template file path where the error occurred.
	Template string
	// Line is the 1-based line number of the error.
	Line int
	// Column is the 1-based column number of the error.
	Column int
	// Variable is the variable name involved in the error.
	Variable string
	// Message is the error message text.
	Message string
}

// dedupeValidationErrors removes duplicate validation errors by keying on
// template, line, column, variable, and message.
func dedupeValidationErrors(in []validator.ValidationResult) []validator.ValidationResult {
	seen := make(map[dedupKey]struct{}, len(in))
	out := make([]validator.ValidationResult, 0, len(in))
	for _, err := range in {
		key := dedupKey{err.Template, err.Line, err.Column, err.Variable, err.Message}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, err)
	}
	return out
}

// normalizePath returns a cleaned, lowercased version of a file path for
// case-insensitive comparison.
func normalizePath(value string) string {
	return filepath.Clean(strings.ToLower(value))
}

// normalizeTemplateKey returns a cleaned, forward-slash-separated template key
// with any leading "./" prefix stripped, for consistent map lookups.
func normalizeTemplateKey(value string) string {
	cleaned := filepath.ToSlash(filepath.Clean(value))
	return strings.TrimPrefix(cleaned, "./")
}
