package validator

import (
	"path/filepath"
	"strings"

	"github.com/abiiranathan/go-template-lsp/gotpl-analyzer/ast"
)

// normalizePath returns a cleaned, lowercased version of a file path for
// case-insensitive comparison.
func normalizePath(value string) string {
	return filepath.ToSlash(filepath.Clean(strings.ToLower(value)))
}

// IsFileBasedPartial determines if a template name refers to a file path
// rather than a named block.
func IsFileBasedPartial(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".html", ".tmpl", ".gohtml", ".tpl", ".htm":
		return true
	}
	return false
}

// isWhitespace checks if a byte is whitespace (space, tab, newline, carriage return).
func isWhitespace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// ValidateTemplateFileStr exposes internal validation for testing.
func ValidateTemplateFileStr(
	content string,
	vars []ast.TemplateVar,
	templateName string,
	baseDir, templateRoot string,
	registry map[string][]NamedBlockEntry,
	funcMaps FuncMapRegistry,
) []ValidationResult {
	varMap := buildVarMap(vars)
	return ValidateTemplateContent(content, varMap, templateName, baseDir, templateRoot, 1, registry, funcMaps)
}

// ValidateNamedBlockContent validates a named block body with a non-default line offset.
func ValidateNamedBlockContent(
	content string,
	vars []ast.TemplateVar,
	templateName string,
	baseDir, templateRoot string,
	lineOffset int,
	registry map[string][]NamedBlockEntry,
	funcMaps FuncMapRegistry,
) []ValidationResult {
	varMap := buildVarMap(vars)
	return ValidateTemplateContent(content, varMap, templateName, baseDir, templateRoot, lineOffset, registry, funcMaps)
}

// ParseAllNamedTemplates exposes named template parsing for testing.
func ParseAllNamedTemplates(baseDir, templateRoot string) (map[string][]NamedBlockEntry, []NamedBlockDuplicateError) {
	return parseAllNamedTemplates(baseDir, templateRoot)
}

// ExtractNamedTemplatesFromContent exposes content extraction for testing.
func ExtractNamedTemplatesFromContent(content, absolutePath, templatePath string, registry map[string][]NamedBlockEntry) {
	extractNamedTemplatesFromContent(content, absolutePath, templatePath, registry)
}

// ParseAllNamedTemplatesFromStore exposes store-backed named template parsing for testing.
func ParseAllNamedTemplatesFromStore(store TemplateStore, baseDir, templateRoot string) (map[string][]NamedBlockEntry, []NamedBlockDuplicateError) {
	return parseAllNamedTemplatesFromStore(store, baseDir, templateRoot)
}

type templateCallWithScope struct {
	target string
	vars   []ast.TemplateVar
}

func collectCallContextsWithScope(
	content string,
	varMap map[string]ast.TemplateVar,
	funcMaps FuncMapRegistry,
) []templateCallWithScope {
	var calls []templateCallWithScope

	var scopeStack []ScopeType
	rootScope := buildRootScope(varMap)
	scopeStack = append(scopeStack, rootScope)

	cur := 0
	for cur < len(content) {
		openRel := strings.Index(content[cur:], "{{")
		if openRel == -1 {
			break
		}
		openIdx := cur + openRel
		closeRel := strings.Index(content[openIdx:], "}}")
		if closeRel == -1 {
			break
		}
		closeIdx := openIdx + closeRel

		contentStart := openIdx + 2
		if contentStart < closeIdx && content[contentStart] == '-' {
			contentStart++
		}
		for contentStart < closeIdx && isWhitespace(content[contentStart]) {
			contentStart++
		}

		contentEnd := closeIdx
		if contentEnd > contentStart && content[contentEnd-1] == '-' {
			contentEnd--
		}
		for contentEnd > contentStart && isWhitespace(content[contentEnd-1]) {
			contentEnd--
		}

		var action string
		if contentStart < contentEnd {
			action = content[contentStart:contentEnd]
		}
		cur = closeIdx + 2

		if strings.Contains(action, "/*") && strings.Contains(action, "*/") {
			continue
		}

		words := strings.Fields(action)
		first := ""
		if len(words) > 0 {
			first = words[0]
			if idx := strings.IndexByte(first, '('); idx != -1 {
				first = first[:idx]
			}
		}

		isElse := first == "else"
		var elseAction string

		if isElse {
			if len(scopeStack) > 1 {
				scopeStack = scopeStack[:len(scopeStack)-1]
			}
			if len(words) > 1 {
				elseAction = words[1]
				if idx := strings.IndexByte(elseAction, '('); idx != -1 {
					elseAction = elseAction[:idx]
				}
			}
		} else if first == "end" {
			if len(scopeStack) > 1 {
				scopeStack = scopeStack[:len(scopeStack)-1]
			}
			continue
		}

		if first == "template" || first == "block" {
			parts := parseTemplateAction(action)
			if len(parts) >= 1 {
				target := parts[0]
				contextArg := "."
				if len(parts) >= 2 {
					contextArg = parts[1]
				}
				partialScope := resolvePartialScope(contextArg, scopeStack, varMap, funcMaps)
				partialVarMap := buildPartialVarMap(contextArg, partialScope, scopeStack, varMap)
				propagatedVars := scopeVarsToTemplateVars(partialVarMap)
				if len(propagatedVars) > 0 {
					calls = append(calls, templateCallWithScope{
						target: target,
						vars:   propagatedVars,
					})
				}
			}
		}

		if first != "range" && first != "with" && first != "if" {
			registerInlineLocalAssignmentsSafe(action, scopeStack, varMap, funcMaps)
		}

		actionToPush := first
		exprToParse := action

		if isElse {
			if elseAction != "" {
				actionToPush = elseAction
				idx := strings.Index(action, words[1])
				if idx != -1 {
					exprToParse = action[idx:]
				}
			} else {
				top := ScopeType{}
				if len(scopeStack) > 0 {
					top = scopeStack[len(scopeStack)-1]
				}
				scopeStack = append(scopeStack, top)
				continue
			}
		}

		switch actionToPush {
		case "range":
			rangeExpr := strings.TrimSpace(strings.TrimPrefix(exprToParse, "range"))
			assignmentNames, rangePipeline, hasAssignment := splitAssignment(rangeExpr)
			if hasAssignment {
				rangeExpr = rangePipeline
			}
			newScope := childScope(createScopeFromRange(rangeExpr, scopeStack, varMap, funcMaps))
			if hasAssignment {
				registerRangeLocalsSafe(&newScope, assignmentNames, rangeExpr, scopeStack, varMap, funcMaps)
			}
			scopeStack = append(scopeStack, newScope)

		case "with":
			withExpr := strings.TrimSpace(strings.TrimPrefix(exprToParse, "with"))
			assignmentNames, withPipeline, hasAssignment := splitAssignment(withExpr)
			if hasAssignment {
				withExpr = withPipeline
			}
			newScope := childScope(createScopeFromWith(withExpr, scopeStack, varMap, funcMaps))
			if hasAssignment {
				registerAssignedLocalsSafe(&newScope, assignmentNames, withExpr, scopeStack, varMap, funcMaps)
			}
			scopeStack = append(scopeStack, newScope)

		case "if":
			top := ScopeType{}
			if len(scopeStack) > 0 {
				top = childScope(scopeStack[len(scopeStack)-1])
			}
			ifExpr := strings.TrimSpace(strings.TrimPrefix(exprToParse, "if"))
			assignmentNames, ifPipeline, hasAssignment := splitAssignment(ifExpr)
			if hasAssignment {
				registerAssignedLocalsSafe(&top, assignmentNames, ifPipeline, scopeStack, varMap, funcMaps)
			}
			scopeStack = append(scopeStack, top)
		}
	}

	return calls
}
