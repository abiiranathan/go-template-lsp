// Package ast performs comprehensive static analysis on Go source code to extract:
//  1. Template render calls with their associated data variables
//  2. Template function maps (custom functions available in templates)
//  3. Template variable definitions from context setters
package ast

import (
	"fmt"
	"go/token"
	"sync"

	"golang.org/x/tools/go/packages"
)

var (
	// globalPkgCache caches loaded packages across runs to speed up incremental builds.
	globalPkgCache   = make(map[string]*AnalysisResult)
	globalPkgCacheMu sync.RWMutex
)

// InvalidateCache evicts all in-memory Go AST cache entries.
func InvalidateCache() {
	globalPkgCacheMu.Lock()
	clear(globalPkgCache)
	globalPkgCacheMu.Unlock()
}

// AnalyzeDir performs static analysis with in-memory caching.
func AnalyzeDir(dir string, contextFile string, config *AnalysisConfig) AnalysisResult {
	ClearTypeCache()

	result := AnalysisResult{}
	fset := token.NewFileSet()

	// Load configuration optimized for template data extraction.
	// Tests: false avoids compiling test binaries and prevents lock contention.
	cfg := &packages.Config{
		Mode: packages.NeedName |
			packages.NeedFiles |
			packages.NeedCompiledGoFiles |
			packages.NeedImports |
			packages.NeedTypes |
			packages.NeedTypesInfo |
			packages.NeedSyntax,
		Dir:   dir,
		Fset:  fset,
		Tests: false,
	}

	// Load all packages in module using `./...` in a single invocation
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("load error: %v", err))
		return result
	}

	info, allFiles := mergeTypeInfo(pkgs, &result)

	filesMap := buildFileMap(allFiles, fset)
	structIndex := buildStructIndex(fset, filesMap)

	fc := newFieldCache()
	seenPool := newSeenMapPool()

	// Collect function scopes concurrently
	scopes := collectFuncScopesOptimized(allFiles, info, fset, structIndex, fc, config, filesMap, seenPool)

	// Extract global implicit variables (e.g. middleware c.Set())
	globalImplicitVars := extractGlobalImplicitVars(scopes)

	// Generate render calls
	result.RenderCalls = generateRenderCalls(scopes, globalImplicitVars, info, fset, dir, structIndex, fc, seenPool)

	// Aggregate template function maps
	result.FuncMaps = aggregateFuncMaps(scopes)

	// Context enrichment from JSON if specified
	if contextFile != "" {
		result.RenderCalls = enrichRenderCallsWithContext(
			result.RenderCalls, contextFile, pkgs, structIndex, fc, fset, config, seenPool,
		)
	}
	return result
}

// extractGlobalImplicitVars identifies template variables that are set outside
// any render call context (e.g. in middleware functions).  These are available
// to every template.
func extractGlobalImplicitVars(scopes []FuncScope) []TemplateVar {
	var globalVars []TemplateVar
	for _, scope := range scopes {
		if len(scope.RenderNodes) == 0 && len(scope.SetVars) > 0 {
			globalVars = append(globalVars, scope.SetVars...)
		}
	}
	return globalVars
}

// aggregateFuncMaps collects all function-map definitions from scopes and
// deduplicates by name.
func aggregateFuncMaps(scopes []FuncScope) []FuncMapInfo {
	total := 0
	for _, scope := range scopes {
		total += len(scope.FuncMaps)
	}

	all := make([]FuncMapInfo, 0, total)
	for _, scope := range scopes {
		all = append(all, scope.FuncMaps...)
	}

	seen := make(map[string]bool, len(all))
	unique := make([]FuncMapInfo, 0, len(all))
	for _, fm := range all {
		if !seen[fm.Name] {
			seen[fm.Name] = true
			unique = append(unique, fm)
		}
	}
	return unique
}
