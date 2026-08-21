package ast

import (
	goast "go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"slices"
	"strings"
)

// resolveRenderCall analyzes a render call expression to extract:
// - Template name(s) being rendered
// - Index of the template name argument
// - Index of the data payload argument
//
// Template names can come from:
// 1. String literals: c.Render("template.html", data), c.HTML(200, "template.html", data)
// 2. Constants: c.Render(TemplateName, data)
// 3. Variables: c.Render(tplName, data)
func resolveRenderCall(call *goast.CallExpr, info *types.Info, stringAssignments map[string][]string) *ResolvedRender {
	if len(call.Args) == 0 {
		return nil
	}

	// Dynamically determine the index of the template argument and data argument
	tplIdx, dataIdx := detectTemplateAndDataArgIndices(call, info, stringAssignments)
	if tplIdx < 0 || tplIdx >= len(call.Args) {
		return nil
	}

	arg := call.Args[tplIdx]

	// Resolve template name(s)
	names := resolveTemplateName(arg, info, stringAssignments)
	if len(names) == 0 {
		return nil
	}

	return &ResolvedRender{
		Node:           call,
		TemplateNames:  names,
		TemplateArgIdx: tplIdx,
		DataArgIdx:     dataIdx,
	}
}

// detectTemplateAndDataArgIndices inspects arguments across various Go web framework
// signatures (Gin, Echo, Fiber, standard library, Chi) to find:
//   - tplIdx:  index of the template path argument
//   - dataIdx: index of the data context payload argument
func detectTemplateAndDataArgIndices(
	call *goast.CallExpr,
	info *types.Info,
	stringAssignments map[string][]string,
) (tplIdx int, dataIdx int) {
	// Pass 1: Look for explicit template file extensions (e.g. "index.html", "user.tmpl")
	for i, arg := range call.Args {
		if s := extractStringFast(arg); s != "" {
			if isTemplateExtension(s) {
				return i, i + 1
			}
		}
	}

	// Pass 2: Look for tracked string variables
	for i, arg := range call.Args {
		if ident, ok := arg.(*goast.Ident); ok {
			if vals, ok := stringAssignments[ident.Name]; ok && len(vals) > 0 {
				return i, i + 1
			}
		}
	}

	// Pass 3: Check framework signature patterns based on string-typed arguments
	for i, arg := range call.Args {
		// Basic string literal
		if lit, ok := arg.(*goast.BasicLit); ok && lit.Kind == token.STRING {
			return i, i + 1
		}

		// Resolved Go type is string
		if info != nil {
			if tv, ok := info.Types[arg]; ok && tv.Type != nil {
				if basic, ok := tv.Type.Underlying().(*types.Basic); ok && basic.Kind() == types.String {
					return i, i + 1
				}
			}
		}
	}

	return -1, -1
}

// isTemplateExtension reports whether a filename has a common Go template extension.
func isTemplateExtension(name string) bool {
	lower := strings.ToLower(name)
	for _, ext := range []string{".html", ".tmpl", ".gohtml", ".tpl", ".htm"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// resolveTemplateName extracts template name(s) from an argument expression.
// Handles string literals, constants, and variables.
func resolveTemplateName(
	arg goast.Expr,
	info *types.Info,
	stringAssignments map[string][]string,
) []string {
	// Try direct string extraction
	if s := extractStringFast(arg); s != "" {
		return []string{s}
	}

	// Try identifier resolution
	ident, ok := arg.(*goast.Ident)
	if !ok {
		return nil
	}

	// Try constant resolution
	if info != nil {
		if obj := info.ObjectOf(ident); obj != nil {
			if c, ok := obj.(*types.Const); ok {
				val := c.Val()
				if val.Kind() == constant.String {
					return []string{constant.StringVal(val)}
				}
			}
		}
	}

	// Try variable resolution
	if vals, ok := stringAssignments[ident.Name]; ok {
		return vals
	}

	return nil
}

// isRenderCall checks if a call expression matches known template render functions
// based on configured function names.
func isRenderCall(call *goast.CallExpr, config *AnalysisConfig) bool {
	funcName := ""

	switch fn := call.Fun.(type) {
	case *goast.SelectorExpr:
		funcName = fn.Sel.Name
	case *goast.Ident:
		funcName = fn.Name
	}

	if funcName == "" {
		return false
	}

	// Check configured names list
	if len(config.RenderFunctionNames) > 0 && slices.Contains(config.RenderFunctionNames, funcName) {
		return true
	}

	return false
}
