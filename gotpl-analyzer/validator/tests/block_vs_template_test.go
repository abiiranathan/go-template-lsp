package validator_test

import (
	"testing"

	"github.com/abiiranathan/go-template-lsp/gotpl-analyzer/ast"
	"github.com/abiiranathan/go-template-lsp/gotpl-analyzer/validator"
)

func TestBlockExecutionInPlace(t *testing.T) {
	// Case 1: {{ block }} with default content must NOT trigger "template not found"
	templateWithBlock := `
<div>
    {{ block "content_area" . }}
        <h1>Default Header: {{ .Title }}</h1>
    {{ end }}
</div>
`

	vars := map[string]ast.TemplateVar{
		"Title": {Name: "Title", TypeStr: "string"},
	}

	errs := validator.ValidateTemplateContent(
		templateWithBlock,
		vars,
		"layout.html",
		".",
		"",
		1,
		nil,
	)

	if len(errs) > 0 {
		for _, e := range errs {
			t.Errorf("Unexpected error on valid block: line %d: %s", e.Line, e.Message)
		}
	}

	// Case 2: {{ template "missing" . }} MUST trigger "template not found"
	templateWithMissing := `
<div>
    {{ template "missing_partial" . }}
</div>
`
	missingErrs := validator.ValidateTemplateContent(
		templateWithMissing,
		vars,
		"page.html",
		".",
		"",
		1,
		nil,
	)

	if len(missingErrs) == 0 {
		t.Fatal("Expected error for missing template 'missing_partial', got 0")
	}

	found := false
	for _, e := range missingErrs {
		if e.Variable == "missing_partial" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Expected error specifically for 'missing_partial', got: %#v", missingErrs)
	}
}
