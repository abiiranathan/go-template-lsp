package validator_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abiiranathan/go-template-lsp/gotpl-analyzer/ast"
	"github.com/abiiranathan/go-template-lsp/gotpl-analyzer/validator"
)

// TestPropagatedRenderVarIndexInsideRange verifies that variable propagation
// through {{ template "cbc-report-row" . }} or {{ block "cbc-report-row" . }}
// inside a {{ range .Notes }} loop propagates the Note element fields, NOT the root fields.
func TestPropagatedRenderVarIndexInsideRange(t *testing.T) {
	parentContent := `
		{{ range .Notes }}
			{{ template "cbc-report-row" . }}
		{{ end }}
	`
	childContent := `
		{{ block "cbc-report-row" . }}
			<span>{{ .Value }}</span>
			<span>{{ .UpperLimit }}</span>
		{{ end }}
	`

	store := validator.TemplateStore{
		filepath.Join(".", "parent.html"): parentContent,
		filepath.Join(".", "child.html"):  childContent,
	}

	renderCalls := []ast.RenderCall{
		{
			File:     "handler.go",
			Line:     10,
			Template: "parent.html",
			Vars: []ast.TemplateVar{
				{
					Name:     "Notes",
					TypeStr:  "[]Note",
					IsSlice:  true,
					ElemType: "Note",
					Fields: []ast.FieldInfo{
						{Name: "Value", TypeStr: "string"},
						{Name: "UpperLimit", TypeStr: "float64"},
					},
				},
			},
		},
	}

	namedBlocks, _ := validator.ParseAllNamedTemplatesFromStore(store, ".", ".")

	idx := validator.BuildPropagatedRenderVarIndex(renderCalls, namedBlocks, ".", ".", nil, store)

	cbcVars, ok := idx["cbc-report-row"]
	if !ok {
		cbcVars, ok = idx["child.html"]
	}
	if !ok {
		t.Fatalf("expected propagated variables for cbc-report-row, got none")
	}

	hasValue := false
	hasUpperLimit := false
	for _, v := range cbcVars {
		if v.Name == "Value" {
			hasValue = true
		}
		if v.Name == "UpperLimit" {
			hasUpperLimit = true
		}
		if v.Name == "." {
			for _, f := range v.Fields {
				if f.Name == "Value" {
					hasValue = true
				}
				if f.Name == "UpperLimit" {
					hasUpperLimit = true
				}
			}
		}
	}

	if !hasValue || !hasUpperLimit {
		t.Fatalf("expected cbc-report-row to receive Value and UpperLimit, got vars: %#v", cbcVars)
	}

	// Validate child content using the propagated vars — must produce ZERO errors!
	varMap := make(map[string]ast.TemplateVar)
	for _, v := range cbcVars {
		varMap[v.Name] = v
	}

	errs := validator.ValidateTemplateContent(childContent, varMap, "child.html", ".", ".", 1, namedBlocks)
	if len(errs) != 0 {
		t.Fatalf("expected 0 validation errors for child.html, got %d: %#v", len(errs), errs)
	}
}

// TestBlockKeywordNotTreatedAsTemplate verifies that unquoted {{ block "name" . }}
// does not generate a synthetic template entry named "block".
func TestBlockKeywordNotTreatedAsTemplate(t *testing.T) {
	content := `
		{{ block "my-named-block" .User }}
			<p>{{ .Name }}</p>
		{{ end }}
	`
	store := validator.TemplateStore{
		filepath.Join(".", "page.html"): content,
	}

	renderCalls := []ast.RenderCall{
		{
			File:     "handler.go",
			Line:     1,
			Template: "page.html",
			Vars: []ast.TemplateVar{
				{
					Name:    "User",
					TypeStr: "User",
					Fields:  []ast.FieldInfo{{Name: "Name", TypeStr: "string"}},
				},
			},
		},
	}

	namedBlocks, _ := validator.ParseAllNamedTemplatesFromStore(store, ".", ".")
	idx := validator.BuildPropagatedRenderVarIndex(renderCalls, namedBlocks, ".", ".", nil, store)

	if _, found := idx["block"]; found {
		t.Fatalf("unexpected synthetic template name 'block' found in propagated render var index")
	}
}

// TestHoverOnQuotedStringLiteral verifies that hovering on a string argument like
// "2026-02-06" returns type 'string' rather than extracting the integer '2026'.
func TestHoverOnQuotedStringLiteral(t *testing.T) {
	content := `{{ .visit.CreatedAt.Format "2026-02-06" }}`

	vars := map[string]ast.TemplateVar{
		"visit": {
			Name:    "visit",
			TypeStr: "*Visit",
			Fields: []ast.FieldInfo{
				{Name: "CreatedAt", TypeStr: "time.Time"},
			},
		},
	}

	// Cursor is on "2026" inside "2026-02-06" (line 1, col 29)
	res := validator.GetHoverResult(
		content,
		vars,
		"test.html",
		".", ".",
		0,
		1, 29,
		nil, nil, nil,
	)

	if res == nil {
		t.Fatalf("expected hover result on string literal, got nil")
	}

	if res.TypeStr != "string" {
		t.Fatalf("expected string type for quoted literal, got %q (expression: %q)", res.TypeStr, res.Expression)
	}
}

// TestHoverDocForExternalMethod verifies that hovering on a method of an
// external type (e.g. time.Time.Format) surfaces the stdlib doc comment,
// resolved end-to-end from the analyzer's type registry.
func TestHoverDocForExternalMethod(t *testing.T) {
	dir := t.TempDir()
	writeModuleFile(t, filepath.Join(dir, "go.mod"), "module example.com/hover\n\ngo 1.21\n")
	writeModuleFile(t, filepath.Join(dir, "main.go"), `package main

import (
	"time"
)

type Visit struct {
	CreatedAt time.Time
}

func Render(w http.ResponseWriter, template string, data interface{}) {}

func main() {
	Render(nil, "test.html", map[string]interface{}{
		"visit": Visit{},
	})
}
`)

	result := ast.AnalyzeDir(dir, "", &ast.DefaultConfig)
	result.Flatten()

	vars := make(map[string]ast.TemplateVar)
	for _, rc := range result.RenderCalls {
		for _, v := range rc.Vars {
			vars[v.Name] = v
		}
	}
	if len(vars) == 0 {
		t.Fatal("expected render vars from analyzer")
	}

	content := `{{ .visit.CreatedAt.Format "2006-01-02" }}`

	// Cursor on "Format" → sub-expression ".visit.CreatedAt.Format".
	// col 26 lands on the final 't' of "Format".
	res := validator.GetHoverResult(
		content,
		vars,
		"test.html",
		".", ".",
		0,
		1, 26,
		nil, nil, result.Types,
	)
	if res == nil {
		t.Fatal("expected hover result on .visit.CreatedAt.Format, got nil")
	}
	if res.TypeStr != "string" {
		t.Errorf("expected Format to return string, got %q", res.TypeStr)
	}
	if !strings.Contains(res.Doc, "Format returns a textual representation") {
		t.Errorf("expected Format doc in hover, got %q", res.Doc)
	}

	// Hover on another external method to confirm docs are not Format-specific.
	res = validator.GetHoverResult(
		`{{ .visit.CreatedAt.Date }}`,
		vars,
		"test.html",
		".", ".",
		0,
		1, 23, // col 23 lands on the final 'e' of "Date"
		nil, nil, result.Types,
	)
	if res == nil {
		t.Fatal("expected hover result on .visit.CreatedAt.Date, got nil")
	}
	if !strings.Contains(res.Doc, "returns the year, month, and day in which t occurs") {
		t.Errorf("expected Date doc in hover, got %q", res.Doc)
	}
}

func writeModuleFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
