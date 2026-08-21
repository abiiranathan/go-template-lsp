package ast

import (
	goast "go/ast"
	"go/token"
	"go/types"
	"slices"
	"strings"
)

// extractSetCallVarOptimized extracts template variable information from
// a context setter call across any framework.
//
// Examples:
//   - c.Set("user", user)        (Echo, Gin, Rex)
//   - c.Locals("user", user)     (Fiber)
//   - ctx.SetVar("user", user)   (Custom)
//
// Extracts: name="user", type, fields, documentation.
func extractSetCallVarOptimized(
	call *goast.CallExpr,
	info *types.Info,
	fset *token.FileSet,
	structIndex map[string]structIndexEntry,
	fc *fieldCache,
	config *AnalysisConfig,
	seenPool *seenMapPool,
) *TemplateVar {
	// Must be method call
	sel, ok := call.Fun.(*goast.SelectorExpr)
	if !ok {
		return nil
	}

	// Verify method name matches any configured setter
	if !isSetterMethod(sel.Sel.Name, config) {
		return nil
	}

	// Verify receiver is a context object
	if !isContextReceiver(sel.X, info, config) {
		return nil
	}

	// Extract variable name (first argument)
	if len(call.Args) < 2 {
		return nil
	}

	key := extractStringFast(call.Args[0])
	if key == "" {
		if ident, ok := call.Args[0].(*goast.Ident); ok && info != nil {
			if obj := info.ObjectOf(ident); obj != nil {
				if c, ok := obj.(*types.Const); ok {
					key = c.Val().String()
					key = strings.Trim(key, `"`)
				}
			}
		}
	}

	if key == "" {
		return nil
	}

	// Build template variable with full type information
	tv := TemplateVar{Name: key}
	valArg := call.Args[1]

	// Extract type information if available
	if typeInfo, ok := info.Types[valArg]; ok && typeInfo.Type != nil {
		tv.TypeStr = normalizeTypeStr(typeInfo.Type)

		seen := seenPool.get()
		tv.Fields, tv.Doc = extractFieldsWithDocs(typeInfo.Type, structIndex, fc, seen, fset)

		// Handle collection types
		tv.IsSlice, tv.ElemType = checkSliceType(typeInfo.Type, structIndex, fc, seen, fset, &tv)
		tv.IsMap, tv.KeyType = checkMapType(typeInfo.Type, structIndex, fc, seen, fset, &tv)

		seenPool.put(seen)
	} else {
		// Fallback: infer basic type from AST
		tv.TypeStr = inferTypeFromAST(valArg)
	}

	// Find definition location
	tv.DefFile, tv.DefLine, tv.DefCol = findDefinitionLocation(valArg, info, fset)

	return &tv
}

// isSetterMethod reports whether methodName is in the configured SetFunctionNames slice.
func isSetterMethod(methodName string, config *AnalysisConfig) bool {
	return slices.Contains(config.SetFunctionNames, methodName)
}

// isContextReceiver flexibly matches context receivers across frameworks by type name or identifier convention.
func isContextReceiver(expr goast.Expr, info *types.Info, config *AnalysisConfig) bool {
	if expr == nil {
		return false
	}

	// Heuristic 1: Match common receiver identifier names
	if ident, ok := expr.(*goast.Ident); ok {
		name := strings.ToLower(ident.Name)
		if name == "c" || name == "ctx" || name == "context" || name == "req" || name == "r" {
			return true
		}
	}

	if info == nil {
		return false
	}

	typeAndValue, ok := info.Types[expr]
	if !ok || typeAndValue.Type == nil {
		return false
	}

	t := typeAndValue.Type

	// Dereference pointer
	if ptr, ok := t.(*types.Pointer); ok {
		t = ptr.Elem()
	}

	typeStr := t.String()
	for _, ctxName := range config.ContextTypeNames {
		if strings.HasSuffix(typeStr, ctxName) {
			return true
		}
	}

	return false
}

// checkSliceType determines if a type is a slice and extracts element type info.
func checkSliceType(
	t types.Type,
	structIndex map[string]structIndexEntry,
	fc *fieldCache,
	seen map[string]bool,
	fset *token.FileSet,
	tv *TemplateVar,
) (isSlice bool, elemType string) {
	elem := getElementType(t)
	if elem == nil {
		return false, ""
	}

	// Clear seen map for element type extraction
	clear(seen)

	tv.Fields, tv.Doc = extractFieldsWithDocsPreservingDoc(elem, structIndex, fc, seen, fset, tv.Doc)
	return true, normalizeTypeStr(elem)
}

// checkMapType determines if a type is a map and extracts key/value type info.
func checkMapType(
	t types.Type,
	structIndex map[string]structIndexEntry,
	fc *fieldCache,
	seen map[string]bool,
	fset *token.FileSet,
	tv *TemplateVar,
) (isMap bool, keyType string) {
	keyT, elemT := getMapTypes(t)
	if keyT == nil || elemT == nil {
		return false, ""
	}

	// Clear seen map for element type extraction
	clear(seen)

	tv.ElemType = normalizeTypeStr(elemT)
	tv.Fields, tv.Doc = extractFieldsWithDocsPreservingDoc(elemT, structIndex, fc, seen, fset, tv.Doc)
	return true, normalizeTypeStr(keyT)
}
