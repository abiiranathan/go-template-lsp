package ast

import (
	goast "go/ast"
	"go/token"
	"go/types"
	"runtime"
	"sync"
)

// collectFuncScopesOptimized efficiently collects template operations from
// all function and variable declaration scopes using concurrent processing.
func collectFuncScopesOptimized(
	files []*goast.File,
	info *types.Info,
	fset *token.FileSet,
	structIndex map[string]structIndexEntry,
	fc *fieldCache,
	config AnalysisConfig,
	filesMap map[string]*goast.File,
	seenPool *seenMapPool,
) []FuncScope {
	funcNodes, mutatorIndex, stringMapIndex := scanFilesSinglePass(files, info)
	if len(funcNodes) == 0 {
		return nil
	}

	return processNodesConcurrently(funcNodes, info, fset, structIndex, fc, config, filesMap, seenPool, mutatorIndex, stringMapIndex)
}

// scanFilesSinglePass iterates over all AST files in 1 pass to identify work units,
// map mutators, and string-map template lookups.
func scanFilesSinglePass(files []*goast.File, info *types.Info) ([]funcWorkUnit, map[string][]*goast.KeyValueExpr, map[string][]string) {
	funcNodes := make([]funcWorkUnit, 0, len(files)*8)
	mutatorIndex := make(map[string][]*goast.KeyValueExpr)
	stringMapIndex := make(map[string][]string)

	for _, f := range files {
		for _, decl := range f.Decls {
			switch node := decl.(type) {
			case *goast.FuncDecl:
				funcNodes = append(funcNodes, funcWorkUnit{node: node})

				// Check for map mutator function signature (e.g., func SetTriageContext(ctx rex.Map, ...))
				if node.Body != nil && node.Type.Params != nil && len(node.Type.Params.List) > 0 {
					firstParam := node.Type.Params.List[0]
					if isMapStringAnyParam(firstParam, info) {
						paramNames := make(map[string]bool, len(firstParam.Names))
						for _, n := range firstParam.Names {
							paramNames[n.Name] = true
						}

						var kvs []*goast.KeyValueExpr
						goast.Inspect(node.Body, func(n goast.Node) bool {
							if assign, ok := n.(*goast.AssignStmt); ok {
								for i, lhs := range assign.Lhs {
									if idx, ok := lhs.(*goast.IndexExpr); ok {
										if recv, ok := idx.X.(*goast.Ident); ok && paramNames[recv.Name] {
											if keyLit, ok := idx.Index.(*goast.BasicLit); ok && keyLit.Kind == token.STRING {
												if i < len(assign.Rhs) {
													kvs = append(kvs, &goast.KeyValueExpr{
														Key:   keyLit,
														Value: assign.Rhs[i],
													})
												}
											}
										}
									}
								}
							}
							return true
						})
						if len(kvs) > 0 {
							mutatorIndex[node.Name.Name] = kvs
						}
					}
				}

				// Inspect body for closures
				if node.Body != nil {
					goast.Inspect(node.Body, func(n goast.Node) bool {
						if lit, ok := n.(*goast.FuncLit); ok {
							funcNodes = append(funcNodes, funcWorkUnit{node: lit})
						}
						return true
					})
				}

			case *goast.GenDecl:
				if node.Tok == token.VAR || node.Tok == token.CONST {
					funcNodes = append(funcNodes, funcWorkUnit{node: node})
				}

				// String map index collection for package-level maps (e.g. var LabViewTemplates = ...)
				if node.Tok == token.VAR {
					for _, spec := range node.Specs {
						vspec, ok := spec.(*goast.ValueSpec)
						if !ok {
							continue
						}
						for i, name := range vspec.Names {
							if i >= len(vspec.Values) {
								continue
							}
							comp, ok := vspec.Values[i].(*goast.CompositeLit)
							if !ok {
								continue
							}

							collectStringMapLiteral(name.Name, comp, info, stringMapIndex)
						}
					}
				}
			}
		}

		// Inspect file for AssignStmts assigning map composite literals (e.g. api.LabViewTemplates = ...)
		goast.Inspect(f, func(n goast.Node) bool {
			if assign, ok := n.(*goast.AssignStmt); ok {
				for i, lhs := range assign.Lhs {
					if i >= len(assign.Rhs) {
						continue
					}
					mapName := extractIdentOrSelectorName(lhs)
					if mapName != "" {
						if comp, ok := assign.Rhs[i].(*goast.CompositeLit); ok {
							collectStringMapLiteral(mapName, comp, info, stringMapIndex)
						}
					}
				}
			}
			return true
		})
	}

	return funcNodes, mutatorIndex, stringMapIndex
}

func collectStringMapLiteral(name string, comp *goast.CompositeLit, info *types.Info, index map[string][]string) {
	if info != nil {
		if tv, ok := info.Types[comp]; ok && tv.Type != nil {
			if !isMapToStringType(tv.Type) {
				return
			}
		} else if !isMapToStringLitType(comp) {
			return
		}
	} else if !isMapToStringLitType(comp) {
		return
	}

	var vals []string
	for _, elt := range comp.Elts {
		if kv, ok := elt.(*goast.KeyValueExpr); ok {
			if s := extractStringFast(kv.Value); s != "" {
				vals = append(vals, s)
			}
		}
	}

	if len(vals) > 0 {
		for _, val := range vals {
			if !sliceContains(index[name], val) {
				index[name] = append(index[name], val)
			}
		}
	}
}

// processNodesConcurrently distributes work units across multiple workers
// and aggregates their results.
func processNodesConcurrently(
	funcNodes []funcWorkUnit,
	info *types.Info,
	fset *token.FileSet,
	structIndex map[string]structIndexEntry,
	fc *fieldCache,
	config AnalysisConfig,
	filesMap map[string]*goast.File,
	seenPool *seenMapPool,
	mutatorIndex map[string][]*goast.KeyValueExpr,
	stringMapIndex map[string][]string,
) []FuncScope {
	numWorkers := max(runtime.NumCPU(), 1)
	chunkSize := (len(funcNodes) + numWorkers - 1) / numWorkers
	resultChan := make(chan []FuncScope, numWorkers)
	var wg sync.WaitGroup

	for w := range numWorkers {
		start := w * chunkSize
		if start >= len(funcNodes) {
			break
		}
		end := min(start+chunkSize, len(funcNodes))
		chunk := funcNodes[start:end]

		wg.Add(1)
		go processChunk(chunk, info, fset, structIndex, fc, config, filesMap, seenPool, mutatorIndex, stringMapIndex, resultChan, &wg)
	}

	go func() {
		wg.Wait()
		close(resultChan)
	}()

	var allScopes []FuncScope
	for scopes := range resultChan {
		allScopes = append(allScopes, scopes...)
	}
	return allScopes
}

// processChunk is the worker function that processes a chunk of AST nodes.
func processChunk(
	chunk []funcWorkUnit,
	info *types.Info,
	fset *token.FileSet,
	structIndex map[string]structIndexEntry,
	fc *fieldCache,
	config AnalysisConfig,
	filesMap map[string]*goast.File,
	seenPool *seenMapPool,
	mutatorIndex map[string][]*goast.KeyValueExpr,
	stringMapIndex map[string][]string,
	resultChan chan<- []FuncScope,
	wg *sync.WaitGroup,
) {
	defer wg.Done()
	localScopes := make([]FuncScope, 0, len(chunk)/2)
	for _, unit := range chunk {
		scope := processFunc(unit.node, info, fset, structIndex, fc, config, filesMap, seenPool, mutatorIndex, stringMapIndex)
		if len(scope.RenderNodes) > 0 || len(scope.SetVars) > 0 || len(scope.FuncMaps) > 0 {
			localScopes = append(localScopes, scope)
		}
	}
	resultChan <- localScopes
}

// isMapToStringType reports whether t is (or unwraps to) a map whose value
// type is the built-in string kind.
func isMapToStringType(t types.Type) bool {
	if t == nil {
		return false
	}
	if named, ok := t.(*types.Named); ok {
		t = named.Underlying()
	}
	m, ok := t.(*types.Map)
	if !ok {
		return false
	}
	basic, ok := m.Elem().Underlying().(*types.Basic)
	return ok && basic.Kind() == types.String
}

// isMapToStringLitType is a best-effort AST-only check: it looks at the
// composite literal's type node for the pattern map[...]string.
func isMapToStringLitType(comp *goast.CompositeLit) bool {
	if comp.Type == nil {
		return false
	}
	mt, ok := comp.Type.(*goast.MapType)
	if !ok {
		return false
	}
	ident, ok := mt.Value.(*goast.Ident)
	return ok && ident.Name == "string"
}

// isMapStringAnyParam reports whether a function parameter's type resolves to
// map[string]interface{} / map[string]any, including named aliases (rex.Map, gin.H, etc.).
func isMapStringAnyParam(field *goast.Field, info *types.Info) bool {
	if info == nil || len(field.Names) == 0 {
		return false
	}
	tv, ok := info.Defs[field.Names[0]]
	if !ok || tv == nil || tv.Type() == nil {
		return false
	}
	t := tv.Type()
	if named, ok := t.(*types.Named); ok {
		t = named.Underlying()
	}
	m, ok := t.(*types.Map)
	if !ok {
		return false
	}
	basic, ok := m.Key().(*types.Basic)
	if !ok || basic.Kind() != types.String {
		return false
	}
	_, isIface := m.Elem().Underlying().(*types.Interface)
	return isIface
}

// applyMapMutatorCall checks whether a call expression invokes a known
// map-mutating helper (present in mutatorIndex) and, if so, merges its
// recorded key/value mutations into the caller's tracked map variable.
func applyMapMutatorCall(
	call *goast.CallExpr,
	scope *FuncScope,
	mutatorIndex map[string][]*goast.KeyValueExpr,
) {
	if len(mutatorIndex) == 0 || len(call.Args) == 0 {
		return
	}

	// Resolve callee name — handles both plain calls and method calls.
	var calleeName string
	switch fn := call.Fun.(type) {
	case *goast.Ident:
		calleeName = fn.Name
	case *goast.SelectorExpr:
		calleeName = fn.Sel.Name
	default:
		return
	}

	kvs, known := mutatorIndex[calleeName]
	if !known {
		return
	}

	// The first argument must be a map variable already tracked in scope.
	firstArg, ok := call.Args[0].(*goast.Ident)
	if !ok {
		return
	}

	existing, tracked := scope.MapAssignments[firstArg.Name]
	if !tracked {
		return
	}

	// Produce a shallow copy of the composite literal with the extra entries
	// appended so that the original AST node is never mutated.
	updated := &goast.CompositeLit{
		Type:   existing.Type,
		Lbrace: existing.Lbrace,
		Rbrace: existing.Rbrace,
		Elts:   make([]goast.Expr, len(existing.Elts), len(existing.Elts)+len(kvs)),
	}
	copy(updated.Elts, existing.Elts)
	for _, kv := range kvs {
		updated.Elts = append(updated.Elts, kv)
	}

	scope.MapAssignments[firstArg.Name] = updated
}
