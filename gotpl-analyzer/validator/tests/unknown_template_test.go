package validator_test

import (
	"testing"

	"github.com/abiiranathan/go-template-lsp/gotpl-analyzer/ast"
	"github.com/abiiranathan/go-template-lsp/gotpl-analyzer/validator"
)

func TestUnknownTemplateCall(t *testing.T) {
	templateContent := `
<div>
    {{ template "partialstext" . }}
</div>
`
	vars := map[string]ast.TemplateVar{
		"User": {Name: "User", TypeStr: "string"},
	}

	errs := validator.ValidateTemplateContent(
		templateContent,
		vars,
		"test.html",
		".",
		"",
		1,
		nil,
	)

	if len(errs) == 0 {
		t.Fatal("Expected error for invoking unknown template 'partialstext', but got 0 errors")
	}

	found := false
	for _, e := range errs {
		if e.Variable == "partialstext" && e.Severity == "error" {
			found = true
			break
		}
	}

	if !found {
		t.Fatalf("Expected error specifically naming 'partialstext', got: %#v", errs)
	}
}
