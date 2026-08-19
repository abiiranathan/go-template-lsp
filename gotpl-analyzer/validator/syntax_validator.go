package validator

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	templateparse "text/template/parse"
)

var (
	// Matches {{ else with ... }} and {{ else range ... }}, capturing the keyword
	// and the trailing expression.
	elseBranchRe = regexp.MustCompile(`\{\{-?\s*else\s+(with|range)([\s\S]*?)-?\}\}`)
	// Matches opening {{ with ... }} and {{ range ... }} block actions so they can
	// be normalized to {{ if ... }} (Go's parser only accepts else-if chains after if).
	withRangeOpenRe = regexp.MustCompile(`\{\{-?\s*(with|range)([\s\S]*?)-?\}\}`)
	// Matches {{ define ... }} inside commands
	nestedDefineRe = regexp.MustCompile(`\{\{-?\s*define\s+["']([^"']+)["']\s*-?\}\}`)
)

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

func ValidateTemplateSyntax(
	content string,
	templateName string,
	funcMaps FuncMapRegistry,
) ([]ValidationResult, *templateparse.Tree) {
	var results []ValidationResult

	funcs := template.FuncMap{}

	for name := range templateBuiltins {
		funcs[name] = func(...any) any { return nil }
	}
	for name := range commonTemplateHelpers {
		funcs[name] = func(...any) any { return nil }
	}

	for name := range funcMaps {
		funcs[name] = func(...any) any { return nil }
	}

	normalizedContent := normalizeForSyntaxParser(content)

	trees, err := templateparse.Parse(templateName, normalizedContent, "{{", "}}", funcs)

	// If funcMaps is nil (dynamic/unspecified), auto-stub undefined functions so syntax parser only checks structure
	for funcMaps == nil && err != nil && strings.Contains(err.Error(), "function") && strings.Contains(err.Error(), "not defined") {
		missingFunc := extractMissingFuncName(err.Error())
		if missingFunc == "" || funcs[missingFunc] != nil {
			break
		}
		funcs[missingFunc] = func(...any) any { return nil }
		trees, err = templateparse.Parse(templateName, normalizedContent, "{{", "}}", funcs)
	}

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

	if tree != nil && tree.Root != nil && funcMaps != nil {
		results = append(results, validateTreeFunctionArity(tree.Root, templateName, content, funcMaps)...)
	}

	return results, tree
}

func extractMissingFuncName(errStr string) string {
	idx := strings.Index(errStr, `function "`)
	if idx == -1 {
		return ""
	}
	start := idx + len(`function "`)
	end := strings.IndexByte(errStr[start:], '"')
	if end == -1 {
		return ""
	}
	return errStr[start : start+end]
}

// normalizeForSyntaxParser normalizes Go 1.22+ features (else with, else range, nested defines)
// into byte-length-identical constructs for text/template/parse so that syntax, nesting, and
// offsets remain exact for line/column reporting purposes.
func normalizeForSyntaxParser(content string) string {
	normalized := content

	// Normalize {{ else with/range EXPR }} -> {{ else if EXPR }}  (space-padded to same length).
	// The expression is preserved so variable assignments like $x := .Foo stay visible to the parser.
	normalized = elseBranchRe.ReplaceAllStringFunc(normalized, func(match string) string {
		sub := elseBranchRe.FindStringSubmatch(match)
		if len(sub) != 3 {
			return match
		}
		dash := ""
		if strings.HasPrefix(match, "{{-") {
			dash = "-"
		}
		endDash := ""
		if strings.HasSuffix(match, "-}}") {
			endDash = "-"
		}

		// "else with" -> "else if"; pad with spaces to compensate for the shorter keyword.
		kw := sub[1]
		rest := sub[2]

		return "{{" + dash + " else if" + strings.Repeat(" ", len(kw)-2) + rest + endDash + "}}"
	})

	// Normalize opening {{ with/range EXPR }} -> {{ if EXPR }} so any {{ else if }}
	// chain introduced above parses correctly (with/range do not accept else-if in Go).
	normalized = withRangeOpenRe.ReplaceAllStringFunc(normalized, func(match string) string {
		sub := withRangeOpenRe.FindStringSubmatch(match)
		if len(sub) != 3 {
			return match
		}
		dash := ""
		if strings.HasPrefix(match, "{{-") {
			dash = "-"
		}
		endDash := ""
		if strings.HasSuffix(match, "-}}") {
			endDash = "-"
		}

		expr := strings.TrimSpace(sub[2])
		// "with $x := pipeline" / "range $i, $v := pipeline": keep only the pipeline.
		// Declarations are tracked by the semantic validator, not the syntax parser.
		if idx := strings.Index(expr, ":="); idx != -1 {
			expr = strings.TrimSpace(expr[idx+2:])
		}

		head := "{{" + dash + " if "
		tail := endDash + "}}"

		pad := max(len(match)-len(head)-len(expr)-len(tail), 1)

		return head + expr + strings.Repeat(" ", pad) + tail
	})

	// Normalize {{ define "name" }} -> {{ if true }}  (space-padded to same length)
	normalized = nestedDefineRe.ReplaceAllStringFunc(normalized, func(match string) string {
		dash := ""
		if strings.HasPrefix(match, "{{-") {
			dash = "-"
		}
		endDash := ""
		if strings.HasSuffix(match, "-}}") {
			endDash = "-"
		}

		head := "{{" + dash + " if true"
		tail := endDash + "}}"

		pad := max(len(match)-len(head)-len(tail), 1)

		return head + strings.Repeat(" ", pad) + tail
	})

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

		// Undefined variables are reported by the semantic validator with better
		// precision (full expression path and correct scope tracking), so skip them here.
		if strings.Contains(msg, "undefined variable") {
			continue
		}

		varName := ""
		if strings.Contains(msg, `function "`) {
			varName = extractMissingFuncName(msg)
		} else if strings.Contains(msg, `undefined variable: `) {
			idx := strings.Index(msg, `undefined variable: `)
			varName = strings.TrimSpace(msg[idx+len(`undefined variable: `):])
			msg = fmt.Sprintf("Template variable %q is not defined in the current scope", varName)
		}

		results = append(results, ValidationResult{
			Template: templateName,
			Line:     lineNum,
			Column:   colNum,
			Variable: varName,
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
		actualArgs := len(cmd.Args) - 1

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
