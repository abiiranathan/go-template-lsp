package validator_test

import (
	"testing"

	"github.com/abiiranathan/go-template-lsp/gotpl-analyzer/ast"
	"github.com/abiiranathan/go-template-lsp/gotpl-analyzer/validator"
)

func TestElseWithValidation(t *testing.T) {
	templateContent := `
{{ if .PrimaryUser }}
    <span>{{ .PrimaryUser.Name }}</span>
{{ else with .BackupUser }}
    <span>{{ .Name }}</span>
{{ else with $guest := .GuestUser }}
    <span>{{ $guest.Name }}</span>
{{ else }}
    <span>Anonymous</span>
{{ end }}
`

	vars := map[string]ast.TemplateVar{
		"PrimaryUser": {
			Name:    "PrimaryUser",
			TypeStr: "User",
			Fields: []ast.FieldInfo{
				{Name: "Name", TypeStr: "string"},
			},
		},
		"BackupUser": {
			Name:    "BackupUser",
			TypeStr: "User",
			Fields: []ast.FieldInfo{
				{Name: "Name", TypeStr: "string"},
			},
		},
		"GuestUser": {
			Name:    "GuestUser",
			TypeStr: "User",
			Fields: []ast.FieldInfo{
				{Name: "Name", TypeStr: "string"},
			},
		},
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

	if len(errs) > 0 {
		for _, e := range errs {
			t.Errorf("Unexpected error: line %d: %s", e.Line, e.Message)
		}
	}
}
