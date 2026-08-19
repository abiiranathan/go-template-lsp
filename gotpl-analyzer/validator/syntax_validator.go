package validator

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	templateparse "text/template/parse"
)

// Regex patterns to normalize `else with` and `else range` for Go's standard parser
// while maintaining exact 1:1 byte lengths for diagnostic offsets.
// "with " (5 bytes)  -> "if   " (5 bytes)
// "range " (6 bytes) -> "if    " (6 bytes)
var (
	elseWithRe  = regexp.MustCompile(`(\{\{-?\s*else\s+)with(\s+)`)
	elseRangeRe = regexp.MustCompile(`(\{\{-?\s*else\s+)range(\s+)`)
)

// Common template helper functions recognized across popular Go frameworks (Sprig, etc.)
var commonTemplateHelpers = map[string]bool{
	"dict":         true,
	"add":          true,
	"sub":          true,
	"mul":          true,
	"div":          true,
	"mod":          true,
	"default":      true,
	"empty":        true,
	"coalesce":     true,
	"ternary":      true,
	"toJson":       true,
	"toPrettyJson": true,
}

// ValidateTemplateSyntax uses Go's official text/template parser to detect:
// - Syntax errors
// - Unclosed actions ({{ without }})
// - Unclosed/misplaced blocks (including {{ else with }}, {{ else range }}, {{ else if }})
// - Unknown functions not present in builtins or FuncMap
// - Function arity (argument counts)
func ValidateTemplateSyntax(
	content string,
	templateName string,
	funcMaps FuncMapRegistry,
) ([]ValidationResult, *templateparse.Tree) {
	var results []ValidationResult

	funcs := template.FuncMap{}

	// 1. Register built-ins and helpers
	for name := range templateBuiltins {
		funcs[name] = func(...any) any { return nil }
	}
	for name := range commonTemplateHelpers {
		funcs[name] = func(...any) any { return nil }
	}

	// 2. Register all user-defined functions discovered from Go source
	for name := range funcMaps {
		funcs[name] = func(...any) any { return nil }
	}

	// 3. Normalize `else with` and `else range` for standard text/template parser
	normalizedContent := normalizeElseBranches(content)

	// 4. Parse using standard text/template/parse
	trees, err := templateparse.Parse(templateName, normalizedContent, "{{", "}}", funcs)
	if err != nil {
		results = append(results, parseTemplateSyntaxError(err.Error(), templateName)...)
		return results, nil
	}

	tree := trees[templateName]
	if tree == nil {
		for _, t := range trees {
			tree = t
			break
		}
	}

	// 5. Validate function argument counts (Arity) if tree parsed successfully
	if tree != nil && tree.Root != nil {
		results = append(results, validateTreeFunctionArity(tree.Root, templateName, content, funcMaps)...)
	}

	return results, tree
}

// normalizeElseBranches replaces `else with` and `else range` with byte-identical
// `else if` tokens so the standard library parser parses the tree without error,
// preserving exact line and column numbers.
func normalizeElseBranches(content string) string {
	if !strings.Contains(content, "else") {
		return content
	}

	// Replace "with " (5 bytes) with "if   " (5 bytes)
	normalized := elseWithRe.ReplaceAllString(content, "${1}if  ${2}")

	// Replace "range " (6 bytes) with "if    " (6 bytes)
	normalized = elseRangeRe.ReplaceAllString(normalized, "${1}if   ${2}")

	return normalized
}

// parseTemplateSyntaxError extracts 1-based line, column, and clean message from Go template parse errors.
func parseTemplateSyntaxError(errStr string, templateName string) []ValidationResult {
	var results []ValidationResult
	lines := strings.SplitSeq(errStr, "\n")

	for line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		msg := line
		lineNum := 1
		colNum := 1

		prefix := fmt.Sprintf("template: %s:", templateName)
		if after, ok := strings.CutPrefix(msg, prefix); ok {
			msg = after
		} else if idx := strings.Index(msg, ": "); idx != -1 && strings.HasPrefix(msg, "template:") {
			msg = msg[idx+2:]
		}

		// Extract line and optional col
		parts := strings.SplitN(msg, ":", 3)
		if len(parts) >= 2 {
			if l, err := strconv.Atoi(strings.TrimSpace(parts[0])); err == nil {
				lineNum = l
				if len(parts) >= 3 {
					if c, err := strconv.Atoi(strings.TrimSpace(parts[1])); err == nil {
						colNum = c
						msg = strings.TrimSpace(parts[2])
					} else {
						msg = strings.TrimSpace(parts[1] + ":" + parts[2])
					}
				} else {
					msg = strings.TrimSpace(parts[1])
				}
			}
		}

		results = append(results, ValidationResult{
			Template: templateName,
			Line:     lineNum,
			Column:   colNum,
			Message:  fmt.Sprintf("Syntax error: %s", msg),
			Severity: "error",
		})
	}

	return results
}

// validateTreeFunctionArity recursively inspects all node types including BranchNode.Pipe
// for `if`, `with`, `range`, `else if`, `else with`, and `else range`.
func validateTreeFunctionArity(
	node templateparse.Node,
	templateName string,
	content string,
	funcMaps FuncMapRegistry,
) []ValidationResult {
	var results []ValidationResult
	if node == nil || funcMaps == nil {
		return results
	}

	switch n := node.(type) {
	case *templateparse.ListNode:
		if n != nil {
			for _, child := range n.Nodes {
				results = append(results, validateTreeFunctionArity(child, templateName, content, funcMaps)...)
			}
		}

	case *templateparse.ActionNode:
		if n.Pipe != nil {
			results = append(results, validatePipeArity(n.Pipe, templateName, content, funcMaps)...)
		}

	case *templateparse.IfNode:
		if n.Pipe != nil {
			results = append(results, validatePipeArity(n.Pipe, templateName, content, funcMaps)...)
		}
		results = append(results, validateTreeFunctionArity(n.List, templateName, content, funcMaps)...)
		results = append(results, validateTreeFunctionArity(n.ElseList, templateName, content, funcMaps)...)

	case *templateparse.WithNode:
		if n.Pipe != nil {
			results = append(results, validatePipeArity(n.Pipe, templateName, content, funcMaps)...)
		}
		results = append(results, validateTreeFunctionArity(n.List, templateName, content, funcMaps)...)
		results = append(results, validateTreeFunctionArity(n.ElseList, templateName, content, funcMaps)...)

	case *templateparse.RangeNode:
		if n.Pipe != nil {
			results = append(results, validatePipeArity(n.Pipe, templateName, content, funcMaps)...)
		}
		results = append(results, validateTreeFunctionArity(n.List, templateName, content, funcMaps)...)
		results = append(results, validateTreeFunctionArity(n.ElseList, templateName, content, funcMaps)...)
	}

	return results
}

func validatePipeArity(
	pipe *templateparse.PipeNode,
	templateName string,
	content string,
	funcMaps FuncMapRegistry,
) []ValidationResult {
	var results []ValidationResult
	if pipe == nil {
		return results
	}

	for cmdIdx, cmd := range pipe.Cmds {
		if len(cmd.Args) == 0 {
			continue
		}

		firstArg := cmd.Args[0]
		ident, ok := firstArg.(*templateparse.IdentifierNode)
		if !ok {
			continue
		}

		fn, exists := funcMaps[ident.Ident]
		if !exists || len(fn.Params) == 0 {
			continue
		}

		isVariadic := false
		lastParam := fn.Params[len(fn.Params)-1]
		if strings.HasPrefix(lastParam.TypeStr, "...") {
			isVariadic = true
		}

		expectedCount := len(fn.Params)
		actualArgs := len(cmd.Args) - 1 // first arg is the function name itself

		// In a pipeline (stage > 0), the piped value is the final argument
		if cmdIdx > 0 {
			actualArgs++
		}

		if isVariadic {
			minArgs := expectedCount - 1
			if actualArgs < minArgs {
				line, col := calculateNodeLineCol(content, int(cmd.Pos))
				results = append(results, ValidationResult{
					Template: templateName,
					Line:     line,
					Column:   col,
					Variable: ident.Ident,
					Message:  fmt.Sprintf("Wrong number of arguments for %q: expected at least %d, got %d", ident.Ident, minArgs, actualArgs),
					Severity: "error",
				})
			}
		} else {
			if actualArgs != expectedCount {
				line, col := calculateNodeLineCol(content, int(cmd.Pos))
				results = append(results, ValidationResult{
					Template: templateName,
					Line:     line,
					Column:   col,
					Variable: ident.Ident,
					Message:  fmt.Sprintf("Wrong number of arguments for %q: expected %d, got %d", ident.Ident, expectedCount, actualArgs),
					Severity: "error",
				})
			}
		}
	}

	return results
}

func calculateNodeLineCol(content string, pos int) (int, int) {
	if pos < 0 || pos > len(content) {
		return 1, 1
	}
	line := 1 + strings.Count(content[:pos], "\n")
	lastNL := strings.LastIndexByte(content[:pos], '\n')
	col := pos - lastNL
	return line, col
}
