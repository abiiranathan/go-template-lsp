package ast

import (
	goast "go/ast"
	"go/token"
	"go/types"
	"path/filepath"
)

// generateRenderCalls transforms collected scope information into structured
// RenderCall entries with full variable information. Each render call is
// associated with:
//   - Source location (file, line, column range)
//   - Template name(s)
//   - Available template variables (local + scope + global)
func generateRenderCalls(
	scopes []FuncScope,
	globalImplicitVars []TemplateVar,
	info *types.Info,
	fset *token.FileSet,
	dir string,
	structIndex map[string]structIndexEntry,
	fc *fieldCache,
	seenPool *seenMapPool,
) []RenderCall {
	// Pre-count total render calls for efficient allocation
	totalRenders := 0
	for _, scope := range scopes {
		totalRenders += len(scope.RenderNodes)
	}

	renderCalls := make([]RenderCall, 0, totalRenders)

	for _, scope := range scopes {
		if len(scope.RenderNodes) == 0 {
			continue
		}

		for _, rr := range scope.RenderNodes {
			call := rr.Node
			templateArgIdx := rr.TemplateArgIdx

			// Skip invalid render calls
			if len(rr.TemplateNames) == 0 ||
				templateArgIdx < 0 ||
				templateArgIdx >= len(call.Args) {
				continue
			}

			templatePathExpr := call.Args[templateArgIdx]

			// Calculate precise column range for template name
			tplNameStartCol, tplNameEndCol := getExprColumnRange(fset, templatePathExpr)

			// Adjust for string literal quotes
			if lit, ok := templatePathExpr.(*goast.BasicLit); ok && lit.Kind == token.STRING {
				tplNameStartCol++ // Skip opening quote
				tplNameEndCol--   // Skip closing quote
			}

			// Process each template name (usually one, but can be multiple from variables)
			for _, templatePath := range rr.TemplateNames {
				if templatePath == "" {
					continue
				}

				// Extract variables from data argument if present
				var localVars []TemplateVar
				dataArgIdx := rr.DataArgIdx

				if dataArgIdx >= 0 && dataArgIdx < len(call.Args) {
					dataArg := call.Args[dataArgIdx]
					seen := seenPool.get()
					localVars = extractUniversalDataVars(dataArg, info, fset, structIndex, fc, seen, &scope)
					seenPool.put(seen)
				}

				// Combine all available variables: local + scope + global
				allVars := make([]TemplateVar, 0, len(localVars)+len(scope.SetVars)+len(globalImplicitVars))
				allVars = append(allVars, localVars...)
				allVars = append(allVars, scope.SetVars...)
				allVars = append(allVars, globalImplicitVars...)

				// Resolve file path relative to analysis root
				pos := fset.Position(call.Pos())
				relFile := resolveRelativePath(pos.Filename, dir)

				renderCalls = append(renderCalls, RenderCall{
					File:                 relFile,
					Line:                 pos.Line,
					Template:             templatePath,
					TemplateNameStartCol: tplNameStartCol,
					TemplateNameEndCol:   tplNameEndCol,
					Vars:                 allVars,
				})
			}
		}
	}

	return renderCalls
}

// extractUniversalDataVars extracts template variables whether the data argument is:
// 1. A map literal (gin.H, fiber.Map, rex.Map, map[string]any)
// 2. A struct literal (PageData{User: u, Title: "Home"})
// 3. A variable holding a struct whose exported fields become top-level template vars
func extractUniversalDataVars(
	expr goast.Expr,
	info *types.Info,
	fset *token.FileSet,
	structIndex map[string]structIndexEntry,
	fc *fieldCache,
	seen map[string]bool,
	scope *FuncScope,
) []TemplateVar {
	if expr == nil {
		return nil
	}

	// 1. Try Map Composite Literal (e.g. map[string]any{"user": u}, fiber.Map{"user": u}, gin.H{"user": u})
	if mapVars := extractMapVars(expr, info, fset, structIndex, fc, seen); len(mapVars) > 0 {
		return mapVars
	}

	// 2. Try Struct Composite Literal (e.g. PageData{User: u, Title: "Home"})
	if structVars := extractStructLitVars(expr, info, fset, structIndex, fc, seen); len(structVars) > 0 {
		return structVars
	}

	// 3. Check tracked local assignments (e.g. data := gin.H{...}; c.HTML(200, "tpl", data))
	if ident, ok := expr.(*goast.Ident); ok {
		if comp, found := scope.MapAssignments[ident.Name]; found {
			clear(seen)
			if mapVars := extractMapVars(comp, info, fset, structIndex, fc, seen); len(mapVars) > 0 {
				return mapVars
			}
		}
		if comp, found := scope.StructAssignments[ident.Name]; found {
			clear(seen)
			if structVars := extractStructLitVars(comp, info, fset, structIndex, fc, seen); len(structVars) > 0 {
				return structVars
			}
		}
	}

	// 4. Type-directed extraction for struct variables passed directly as root data
	// e.g. user := GetUser(); c.Render("profile.html", user) -> exposes .Name, .Email, etc.
	if info != nil {
		if typeInfo, ok := info.Types[expr]; ok && typeInfo.Type != nil {
			t := typeInfo.Type
			if ptr, ok := t.(*types.Pointer); ok {
				t = ptr.Elem()
			}

			// Only extract if it is a concrete struct type
			if _, isStruct := t.Underlying().(*types.Struct); isStruct {
				clear(seen)
				fields, _ := extractFieldsWithDocs(t, structIndex, fc, seen, fset)
				vars := make([]TemplateVar, 0, len(fields))
				for _, f := range fields {
					vars = append(vars, FieldInfoToTemplateVar(f))
				}
				return vars
			}
		}
	}

	return nil
}

// extractStructLitVars extracts variables from a struct composite literal.
func extractStructLitVars(
	expr goast.Expr,
	info *types.Info,
	fset *token.FileSet,
	structIndex map[string]structIndexEntry,
	fc *fieldCache,
	seen map[string]bool,
) []TemplateVar {
	comp, ok := expr.(*goast.CompositeLit)
	if !ok {
		return nil
	}

	vars := make([]TemplateVar, 0, len(comp.Elts))

	for _, elt := range comp.Elts {
		kv, ok := elt.(*goast.KeyValueExpr)
		if !ok {
			continue
		}

		keyIdent, ok := kv.Key.(*goast.Ident)
		if !ok {
			continue
		}

		name := keyIdent.Name
		tv := TemplateVar{Name: name}

		if typeInfo, ok := info.Types[kv.Value]; ok && typeInfo.Type != nil {
			clear(seen)
			tv.TypeStr = normalizeTypeStr(typeInfo.Type)
			tv.Fields, tv.Doc = extractFieldsWithDocs(typeInfo.Type, structIndex, fc, seen, fset)

			if elemType := getElementType(typeInfo.Type); elemType != nil {
				tv.IsSlice = true
				tv.ElemType = normalizeTypeStr(elemType)
				tv.Fields, tv.Doc = extractFieldsWithDocsPreservingDoc(elemType, structIndex, fc, seen, fset, tv.Doc)
			} else if keyType, elemType := getMapTypes(typeInfo.Type); keyType != nil && elemType != nil {
				tv.IsMap = true
				tv.KeyType = normalizeTypeStr(keyType)
				tv.ElemType = normalizeTypeStr(elemType)
				tv.Fields, tv.Doc = extractFieldsWithDocsPreservingDoc(elemType, structIndex, fc, seen, fset, tv.Doc)
			}
		} else {
			tv.TypeStr = inferTypeFromAST(kv.Value)
		}

		tv.DefFile, tv.DefLine, tv.DefCol = findDefinitionLocation(kv.Value, info, fset)
		vars = append(vars, tv)
	}

	return vars
}

// FieldInfoToTemplateVar converts a FieldInfo record into a TemplateVar.
func FieldInfoToTemplateVar(f FieldInfo) TemplateVar {
	return TemplateVar{
		Name:     f.Name,
		TypeStr:  f.TypeStr,
		Fields:   f.Fields,
		IsSlice:  f.IsSlice,
		IsMap:    f.IsMap,
		KeyType:  f.KeyType,
		ElemType: f.ElemType,
		DefFile:  f.DefFile,
		DefLine:  f.DefLine,
		DefCol:   f.DefCol,
		Doc:      f.Doc,
	}
}

// resolveRelativePath attempts to convert an absolute path to a path
// relative to the specified directory. Falls back to the original path
// if conversion fails.
func resolveRelativePath(absPath, baseDir string) string {
	if abs, err := filepath.Abs(absPath); err == nil {
		if rel, err := filepath.Rel(baseDir, abs); err == nil {
			return rel
		}
	}
	return absPath
}

// getExprColumnRange calculates the precise column span of an AST expression.
// This is used for accurate editor highlighting and navigation features.
func getExprColumnRange(fset *token.FileSet, expr goast.Expr) (startCol, endCol int) {
	pos := fset.Position(expr.Pos())
	endPos := fset.Position(expr.End())
	return pos.Column, endPos.Column
}
