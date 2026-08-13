/*
Package validator performs static analysis on Go templates to identify potential issues.

It analyzes template render calls, discovers template function maps, and validates
template usages against their defined contexts. This package also includes
functionality to extract and validate named template blocks (`define` and `block` actions).
*/
package validator

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"github.com/abiiranathan/go-template-lsp/gotpl-analyzer/ast"
)

// TemplateStore caches template file contents in memory to prevent redundant disk reads.
type TemplateStore map[string]string

// ValidateTemplates validates all templates against their render calls AND
// independently validates every template file and named block discovered by
// walking the full template directory tree.
func ValidateTemplates(
	renderCalls []ast.RenderCall,
	funcMaps []ast.FuncMapInfo,
	baseDir string,
	templateRoot string,
) ([]ValidationResult, map[string][]NamedBlockEntry, []NamedBlockDuplicateError) {
	funcMapRegistry := BuildFuncMapRegistry(funcMaps)

	store := LoadTemplateStore(baseDir, templateRoot)

	namedBlocks, namedBlockErrors := parseAllNamedTemplatesFromStore(store, baseDir, templateRoot)

	// Build template-name → merged var list WITH partial/block propagation
	renderVarsByTemplate := BuildPropagatedRenderVarIndex(renderCalls, namedBlocks, baseDir, templateRoot, funcMapRegistry, store)

	partialTargets := findPartialTargetsFromStore(store)

	renderErrors := validateRenderCallsConcurrently(store, renderCalls, baseDir, templateRoot, namedBlocks, partialTargets, funcMapRegistry)

	treeErrors := validateTemplateTreeFromStore(store, baseDir, templateRoot, namedBlocks, renderVarsByTemplate, partialTargets, funcMapRegistry)

	blockErrors := validateOrphanedNamedBlocks(namedBlocks, renderVarsByTemplate, baseDir, templateRoot, partialTargets, funcMapRegistry)

	allErrors := append(renderErrors, treeErrors...)
	allErrors = append(allErrors, blockErrors...)

	return allErrors, namedBlocks, namedBlockErrors
}

func BuildFuncMapRegistry(funcMaps []ast.FuncMapInfo) FuncMapRegistry {
	registry := make(FuncMapRegistry, len(funcMaps))
	for _, funcMap := range funcMaps {
		registry[funcMap.Name] = funcMap
	}
	return registry
}

var templateRegex = regexp.MustCompile(`\{\{-?\s*(?:template|block|define)\s+["'\x60]([^"'\x60]+)["'\x60]`)

// FindPartialTargets scans all template files to find targets of {{template "..."}} or {{block "..."}} calls.
// Exported for daemon and external callers.
func FindPartialTargets(baseDir, templateRoot string) map[string]bool {
	store := LoadTemplateStore(baseDir, templateRoot)
	return findPartialTargetsFromStore(store)
}

func findPartialTargetsFromStore(store TemplateStore) map[string]bool {
	targets := make(map[string]bool)
	for _, content := range store {
		matches := templateRegex.FindAllStringSubmatch(content, -1)
		for _, m := range matches {
			targets[m[1]] = true
		}
	}
	return targets
}

// LoadTemplateStore performs a single filepath.WalkDir and reads all template files into memory.
func LoadTemplateStore(baseDir, templateRoot string) TemplateStore {
	store := make(TemplateStore)
	root := filepath.Join(baseDir, templateRoot)

	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if IsFileBasedPartial(path) {
			if content, err := os.ReadFile(path); err == nil {
				store[path] = string(content)
			}
		}
		return nil
	})
	return store
}

// BuildPropagatedRenderVarIndex creates a template-name → merged TemplateVar map.
// It starts with direct RenderCall variables and propagates variables through
// {{ template "name" ctx }} and {{ block "name" ctx }} calls until convergence.
func BuildPropagatedRenderVarIndex(
	renderCalls []ast.RenderCall,
	namedBlocks map[string][]NamedBlockEntry,
	baseDir, templateRoot string,
	funcMaps FuncMapRegistry,
	store TemplateStore,
) map[string][]ast.TemplateVar {
	idx := buildRenderVarIndex(renderCalls)

	type queueItem struct {
		name string
	}

	queue := make([]queueItem, 0, len(idx))
	inQueue := make(map[string]bool)

	for name := range idx {
		if name == "block" || name == "" {
			continue
		}
		queue = append(queue, queueItem{name: name})
		inQueue[name] = true
	}

	getContent := func(name string) string {
		absPath := filepath.Join(baseDir, templateRoot, name)
		if content, ok := store[absPath]; ok {
			return content
		}
		for storeAbs, content := range store {
			if strings.HasSuffix(normalizePath(storeAbs), normalizePath(name)) {
				return content
			}
		}
		if entries, ok := namedBlocks[name]; ok && len(entries) > 0 {
			return entries[0].Content
		}
		return ""
	}

	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]
		inQueue[item.name] = false

		currentVars := idx[item.name]
		if len(currentVars) == 0 {
			continue
		}

		content := getContent(item.name)
		if content == "" {
			continue
		}

		varMap := buildVarMap(currentVars)
		calls := collectCallContextsWithScope(content, varMap, funcMaps)

		for _, call := range calls {
			targetName := call.target
			if targetName == "block" || targetName == "" {
				continue
			}

			propagatedVars := call.vars
			if len(propagatedVars) == 0 {
				continue
			}

			changed := mergeVarsIntoIndex(idx, targetName, propagatedVars)

			if entries, ok := namedBlocks[targetName]; ok && len(entries) > 0 {
				relPath := entries[0].TemplatePath
				if relPath != "" && relPath != targetName {
					if mergeVarsIntoIndex(idx, relPath, propagatedVars) {
						changed = true
					}
				}
			}

			if changed && !inQueue[targetName] {
				queue = append(queue, queueItem{name: targetName})
				inQueue[targetName] = true
			}
		}
	}

	delete(idx, "block")
	delete(idx, "")
	return idx
}

func mergeVarsIntoIndex(idx map[string][]ast.TemplateVar, key string, newVars []ast.TemplateVar) bool {
	existing := idx[key]
	seen := make(map[string]bool, len(existing)+len(newVars))
	for _, v := range existing {
		seen[v.Name] = true
	}

	changed := false
	for _, v := range newVars {
		if !seen[v.Name] {
			seen[v.Name] = true
			existing = append(existing, v)
			changed = true
		}
	}

	if changed {
		idx[key] = existing
	}
	return changed
}

func parseAllNamedTemplatesFromStore(store TemplateStore, baseDir, templateRoot string) (map[string][]NamedBlockEntry, []NamedBlockDuplicateError) {
	root := filepath.Join(baseDir, templateRoot)
	registry := make(map[string][]NamedBlockEntry)

	for path, content := range store {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)

		local := make(map[string][]NamedBlockEntry)
		extractNamedTemplatesFromContent(content, path, rel, local)

		for name, entries := range local {
			registry[name] = append(registry[name], entries...)
		}
	}

	errors := detectDuplicateBlocks(registry)
	return registry, errors
}

// buildRenderVarIndex creates a lookup: template-name → merged TemplateVar list.
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

// validateTemplateTreeFromStore iterates over in-memory template entries and validates
// un-rendered/non-partial templates without disk I/O.
func validateTemplateTreeFromStore(
	store TemplateStore,
	baseDir string,
	templateRoot string,
	namedBlocks map[string][]NamedBlockEntry,
	renderVarsByTemplate map[string][]ast.TemplateVar,
	partialTargets map[string]bool,
	funcMaps FuncMapRegistry,
) []ValidationResult {
	root := filepath.Join(baseDir, templateRoot)

	type workItem struct {
		absPath string
		relName string
		content string
		vars    []ast.TemplateVar
	}

	var items []workItem
	for absPath, content := range store {
		rel, err := filepath.Rel(root, absPath)
		if err != nil {
			rel = absPath
		}
		rel = filepath.ToSlash(rel)

		// Skip files that are direct render-call targets — already validated
		if isCoveredByRenderCall(rel, renderVarsByTemplate) {
			continue
		}

		// Skip files that are used as partials — validated via their callers
		if partialTargets[rel] {
			continue
		}

		items = append(items, workItem{
			absPath: absPath,
			relName: rel,
			content: content,
			vars:    renderVarsByTemplate[rel],
		})
	}

	if len(items) == 0 {
		return nil
	}

	return runWorkers(len(items), func(chunk []int) []ValidationResult {
		var errs []ValidationResult
		for _, i := range chunk {
			item := items[i]
			varMap := buildVarMap(item.vars)
			effectiveRegistry := mergeNamedBlockRegistry(namedBlocks, item.content, item.relName)
			errs = append(errs, validateTemplateContentWithRegistry(
				item.content, varMap, item.relName,
				baseDir, templateRoot, 1, effectiveRegistry, funcMaps,
			)...)
		}
		return errs
	})
}

func isCoveredByRenderCall(rel string, renderVarsByTemplate map[string][]ast.TemplateVar) bool {
	if _, ok := renderVarsByTemplate[rel]; ok {
		return true
	}
	normalizedRel := filepath.ToSlash(filepath.Clean(rel))
	normalizedRel = strings.TrimPrefix(normalizedRel, "./")
	for key := range renderVarsByTemplate {
		normalizedKey := filepath.ToSlash(filepath.Clean(key))
		normalizedKey = strings.TrimPrefix(normalizedKey, "./")
		if normalizedRel == normalizedKey {
			return true
		}
		if strings.HasSuffix(normalizedRel, normalizedKey) || strings.HasSuffix(normalizedKey, normalizedRel) {
			return true
		}
	}
	return false
}

// validateOrphanedNamedBlocks validates every {{define}} / {{block}} entry in
// the registry that does NOT have a corresponding render call target AND is NOT
// used as a partial.
func validateOrphanedNamedBlocks(
	namedBlocks map[string][]NamedBlockEntry,
	renderVarsByTemplate map[string][]ast.TemplateVar,
	baseDir string,
	templateRoot string,
	partialTargets map[string]bool,
	funcMaps FuncMapRegistry,
) []ValidationResult {
	type workItem struct {
		entry NamedBlockEntry
		vars  []ast.TemplateVar
	}

	var items []workItem
	for name, entries := range namedBlocks {
		if _, covered := renderVarsByTemplate[name]; covered {
			continue
		}

		if partialTargets[name] {
			continue
		}

		for _, entry := range entries {
			items = append(items, workItem{
				entry: entry,
				vars:  renderVarsByTemplate[name],
			})
		}
	}

	if len(items) == 0 {
		return nil
	}

	return runWorkers(len(items), func(chunk []int) []ValidationResult {
		var errs []ValidationResult
		for _, i := range chunk {
			item := items[i]
			varMap := buildVarMap(item.vars)
			errs = append(errs, ValidateTemplateContent(
				item.entry.Content,
				varMap,
				item.entry.TemplatePath,
				baseDir,
				templateRoot,
				item.entry.Line,
				namedBlocks,
				funcMaps,
			)...)
		}
		return errs
	})
}

func runWorkers(total int, fn func([]int) []ValidationResult) []ValidationResult {
	numWorkers := max(runtime.NumCPU(), 1)
	chunkSize := (total + numWorkers - 1) / numWorkers

	resultChan := make(chan []ValidationResult, numWorkers)
	var wg sync.WaitGroup

	for w := 0; w < numWorkers; w++ {
		start := w * chunkSize
		if start >= total {
			break
		}
		end := min(start+chunkSize, total)

		indices := make([]int, end-start)
		for j := range indices {
			indices[j] = start + j
		}

		wg.Add(1)
		go func(idx []int) {
			defer wg.Done()
			resultChan <- fn(idx)
		}(indices)
	}

	go func() {
		wg.Wait()
		close(resultChan)
	}()

	var all []ValidationResult
	for errs := range resultChan {
		all = append(all, errs...)
	}
	return all
}

func validateRenderCallsConcurrently(
	store TemplateStore,
	renderCalls []ast.RenderCall,
	baseDir string,
	templateRoot string,
	namedBlocks map[string][]NamedBlockEntry,
	partialTargets map[string]bool,
	funcMaps FuncMapRegistry,
) []ValidationResult {
	if len(renderCalls) == 0 {
		return nil
	}

	renderVarsByTemplate := buildRenderVarIndex(renderCalls)

	type workItem struct {
		template string
		vars     []ast.TemplateVar
		rc       ast.RenderCall
	}

	seen := make(map[string]bool)
	var items []workItem
	for _, rc := range renderCalls {
		if seen[rc.Template] {
			continue
		}
		seen[rc.Template] = true
		if _, isNamedBlock := namedBlocks[rc.Template]; isNamedBlock && partialTargets[rc.Template] {
			continue
		}
		items = append(items, workItem{
			template: rc.Template,
			vars:     renderVarsByTemplate[rc.Template],
			rc:       rc,
		})
	}

	return runWorkers(len(items), func(chunk []int) []ValidationResult {
		var errors []ValidationResult
		for _, i := range chunk {
			item := items[i]
			templatePath := filepath.Join(baseDir, templateRoot, item.template)

			var rcErrors []ValidationResult
			if content, ok := store[templatePath]; ok {
				varMap := buildVarMap(item.vars)
				effectiveRegistry := mergeNamedBlockRegistry(namedBlocks, content, item.template)
				rcErrors = validateTemplateContentWithRegistry(
					content, varMap, item.template,
					baseDir, templateRoot, 1, effectiveRegistry, funcMaps,
				)
			} else {
				rcErrors = ValidateTemplateFile(
					templatePath, item.vars, item.template, baseDir, templateRoot, namedBlocks, funcMaps,
				)
			}

			for j := range rcErrors {
				rcErrors[j].GoFile = item.rc.File
				rcErrors[j].GoLine = item.rc.Line
				rcErrors[j].TemplateNameStartCol = item.rc.TemplateNameStartCol
				rcErrors[j].TemplateNameEndCol = item.rc.TemplateNameEndCol
			}
			errors = append(errors, rcErrors...)
		}
		return errors
	})
}

var validTemplateName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

func ValidateTemplateFile(
	templatePath string,
	vars []ast.TemplateVar,
	templateName string,
	baseDir, templateRoot string,
	registry map[string][]NamedBlockEntry,
	funcMaps ...FuncMapRegistry,
) []ValidationResult {
	effectiveFuncMaps := optionalFuncMapRegistry(funcMaps...)

	if entry, ok := findOverlayTemplateEntry(registry, templateName); ok {
		varMap := buildVarMap(vars)
		effectiveRegistry := mergeNamedBlockRegistry(registry, entry.Content, entry.TemplatePath)
		return validateTemplateContentWithRegistry(
			entry.Content, varMap, entry.TemplatePath,
			baseDir, templateRoot, 1, effectiveRegistry, effectiveFuncMaps,
		)
	}

	content, err := os.ReadFile(templatePath)
	if err != nil {
		if entries, ok := registry[templateName]; ok && len(entries) > 0 {
			varMap := buildVarMap(vars)
			entry := entries[0]
			effectiveRegistry := mergeNamedBlockRegistry(registry, entry.Content, entry.TemplatePath)
			return validateTemplateContentWithRegistry(
				entry.Content, varMap, entry.TemplatePath,
				baseDir, templateRoot, entry.Line, effectiveRegistry, effectiveFuncMaps,
			)
		}

		if !validTemplateName.MatchString(templateName) {
			return []ValidationResult{}
		}

		return []ValidationResult{{
			Template: templateName, Line: 1, Column: 1,
			Message:  fmt.Sprintf("Template or named block not found: %s", templateName),
			Severity: "error",
		}}
	}

	varMap := buildVarMap(vars)
	contentStr := string(content)

	// Merge once here; all recursive calls through validateTemplateContentWithRegistry
	// will use this registry without re-merging.
	effectiveRegistry := mergeNamedBlockRegistry(registry, contentStr, templateName)

	return validateTemplateContentWithRegistry(
		contentStr, varMap, templateName,
		baseDir, templateRoot, 1, effectiveRegistry, effectiveFuncMaps,
	)
}

func findOverlayTemplateEntry(registry map[string][]NamedBlockEntry, templateName string) (NamedBlockEntry, bool) {
	entries, ok := registry[templateName]
	if !ok {
		return NamedBlockEntry{}, false
	}

	for _, entry := range entries {
		if entry.Name == templateName && entry.TemplatePath == templateName {
			return entry, true
		}
	}

	return NamedBlockEntry{}, false
}

// buildVarMap converts a slice of TemplateVar to a map for O(1) lookup.
func buildVarMap(vars []ast.TemplateVar) map[string]ast.TemplateVar {
	varMap := make(map[string]ast.TemplateVar, len(vars))
	for _, v := range vars {
		varMap[v.Name] = v
	}
	return varMap
}
