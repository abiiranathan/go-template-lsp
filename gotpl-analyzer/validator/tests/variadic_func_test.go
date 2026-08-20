package validator_test

import (
	"strings"
	"testing"

	"github.com/abiiranathan/go-template-lsp/gotpl-analyzer/ast"
	"github.com/abiiranathan/go-template-lsp/gotpl-analyzer/validator"
)

func TestVariadicFunctionValidation(t *testing.T) {
	funcMaps := validator.FuncMapRegistry{
		"hasPermission": ast.FuncMapInfo{
			Name: "hasPermission",
			Params: []ast.ParamInfo{
				{Name: "user", TypeStr: "User"},
				{Name: "roles", TypeStr: "...string"},
			},
			Returns: []ast.ParamInfo{{TypeStr: "bool"}},
		},
		"joinStrings": ast.FuncMapInfo{
			Name: "joinStrings",
			Params: []ast.ParamInfo{
				{Name: "sep", TypeStr: "string"},
				{Name: "elems", TypeStr: "...string"},
			},
			Returns: []ast.ParamInfo{{TypeStr: "string"}},
		},
	}

	vars := []ast.TemplateVar{
		{
			Name:    "CurrentUser",
			TypeStr: "User",
			Fields: []ast.FieldInfo{
				{Name: "Name", TypeStr: "string"},
			},
		},
	}

	tests := []struct {
		name        string
		content     string
		expectError bool
		errContains string
	}{
		{
			name:        "variadic with 0 optional args",
			content:     `{{ if hasPermission .CurrentUser }}OK{{ end }}`,
			expectError: false,
		},
		{
			name:        "variadic with 1 optional string arg",
			content:     `{{ if hasPermission .CurrentUser "admin" }}OK{{ end }}`,
			expectError: false,
		},
		{
			name:        "variadic with multiple optional string args",
			content:     `{{ if hasPermission .CurrentUser "admin" "editor" "viewer" }}OK{{ end }}`,
			expectError: false,
		},
		{
			name:        "variadic with wrong optional arg type",
			content:     `{{ if hasPermission .CurrentUser 123 }}OK{{ end }}`,
			expectError: true,
			errContains: `expected string, got int`,
		},
		{
			name:        "variadic with too few required args",
			content:     `{{ if hasPermission }}OK{{ end }}`,
			expectError: true,
			errContains: `expected at least 1, got 0`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := validator.ValidateTemplateFileStr(
				tt.content,
				vars,
				"test.html",
				t.TempDir(),
				"",
				nil,
				funcMaps,
			)

			if tt.expectError {
				if len(errs) == 0 {
					t.Fatalf("expected error containing %q, got none", tt.errContains)
				}
				found := false
				for _, e := range errs {
					if strings.Contains(e.Message, tt.errContains) {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("expected error message containing %q, got: %v", tt.errContains, errs)
				}
			} else {
				if len(errs) > 0 {
					t.Fatalf("unexpected validation errors: %v", errs)
				}
			}
		})
	}
}
