package validator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/abiiranathan/go-template-lsp/gotpl-analyzer/ast"
)

// validateTemplateCallWithRegistry validates a {{ template "name" . }} or {{ block "name" . }} invocation.
// It verifies:
// 1. That the target template or named block actually exists (in registry or on disk).
// 2. That the context argument expression is valid in the caller's scope.
// 3. That the target template body is valid when evaluated against the passed context.
func validateTemplateCallWithRegistry(
	action string,
	scopeStack []ScopeType,
	varMap map[string]ast.TemplateVar,
	actualLineNum int,
	col int,
	templateName string,
	baseDir string,
	templateRoot string,
	registry map[string][]NamedBlockEntry,
	funcMaps FuncMapRegistry,
) []ValidationResult {
	var errors []ValidationResult
	parts := parseTemplateAction(action)

	if len(parts) < 1 {
		return errors
	}

	tmplName := parts[0]
	var contextArg string
	if len(parts) >= 2 {
		contextArg = parts[1]
	}

	// 1. Validate the context argument expression in the caller's current scope
	if contextArg != "" && contextArg != "." && contextArg != "$" {
		if err := validateContextArg(contextArg, scopeStack, varMap, funcMaps); err != nil {
			err.Template = templateName
			err.Line = actualLineNum
			err.Column = max(col+strings.Index(action, contextArg), col)
			errors = append(errors, *err)
			return errors
		}
	}

	pinCallSite := func(inner []ValidationResult) []ValidationResult {
		for i := range inner {
			e := &inner[i]
			e.Message = fmt.Sprintf(
				`[in named template %q @ %s] %s`,
				tmplName, e.Template, e.Message,
			)
			if e.Template != templateName {
				e.SourceTemplate = e.Template
				e.SourceLine = e.Line
				e.SourceColumn = e.Column
				e.Template = templateName
				e.Line = actualLineNum
				e.Column = col
			}
		}
		return inner
	}

	// 2. Case A: Target is a named block defined across the project
	if entries, ok := registry[tmplName]; ok && len(entries) > 0 {
		anyValid := false
		allErrors := make([]ValidationResult, 0)
		for _, nt := range entries {
			partialScope := resolvePartialScope(contextArg, scopeStack, varMap, funcMaps)
			partialVarMap := buildPartialVarMap(contextArg, partialScope, scopeStack, varMap)
			partialErrors := validateTemplateContentWithRegistry(
				nt.Content,
				partialVarMap,
				nt.TemplatePath,
				baseDir,
				templateRoot,
				nt.Line,
				registry,
				funcMaps,
			)
			if len(partialErrors) == 0 {
				anyValid = true
			}
			allErrors = append(allErrors, pinCallSite(partialErrors)...)
		}
		if !anyValid {
			errors = append(errors, allErrors...)
		}
		return errors
	}

	// 3. Case B: Target is a file on disk (e.g. "partials/header.html" or "partials/header")
	candidates := []string{
		filepath.Join(baseDir, templateRoot, tmplName),
		filepath.Join(baseDir, templateRoot, tmplName+".html"),
		filepath.Join(baseDir, templateRoot, tmplName+".tmpl"),
		filepath.Join(baseDir, templateRoot, tmplName+".tpl"),
		filepath.Join(baseDir, templateRoot, tmplName+".gohtml"),
	}

	var fullPath string
	foundFile := false
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			foundFile = true
			fullPath = candidate
			break
		}
	}

	if foundFile {
		partialScope := resolvePartialScope(contextArg, scopeStack, varMap, funcMaps)
		partialVarMap := buildPartialVarMap(contextArg, partialScope, scopeStack, varMap)

		partialErrors := ValidateTemplateFile(
			fullPath,
			scopeVarsToTemplateVars(partialVarMap),
			tmplName,
			baseDir,
			templateRoot,
			registry,
			funcMaps,
		)
		errors = append(errors, pinCallSite(partialErrors)...)
		return errors
	}

	// 4. Case C: Target does NOT exist anywhere -> Report error!
	nameCol := col
	if idx := strings.Index(action, `"`+tmplName+`"`); idx != -1 {
		nameCol = col + idx + 1 // Point inside the quotes
	} else if idx := strings.Index(action, tmplName); idx != -1 {
		nameCol = col + idx
	}

	errors = append(errors, ValidationResult{
		Template: templateName,
		Line:     actualLineNum,
		Column:   nameCol,
		Variable: tmplName,
		Message:  fmt.Sprintf(`Template or named block %q is not defined (not found as a template file or {{ define %q }})`, tmplName, tmplName),
		Severity: "error",
	})

	return errors
}
