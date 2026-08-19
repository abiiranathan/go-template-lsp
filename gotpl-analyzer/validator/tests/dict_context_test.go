package validator_test

import (
	"testing"

	"github.com/abiiranathan/go-template-lsp/gotpl-analyzer/ast"
	"github.com/abiiranathan/go-template-lsp/gotpl-analyzer/validator"
)

const dictContextTemplate = `{{ define "roles" }}
{{ $value := index . "username" }}

{{ template "profile" dict "name" "Abiira" "age" 30 }}
{{ end}}

{{ define "role_item" }}
{{ .key }}: {{ .value }}
{{ end }}

{{ define "profile" }}
Context: {{ . }}
Name: {{ .name }}
Age: {{ .age }}
{{ end }}`

func dictContextRegistry(t *testing.T) (map[string][]validator.NamedBlockEntry, string) {
	t.Helper()
	registry := map[string][]validator.NamedBlockEntry{}
	validator.ExtractNamedTemplatesFromContent(dictContextTemplate, "views/partials/roles.html", "views/partials/roles.html", registry)
	roles, ok := registry["roles"]
	if !ok || len(roles) == 0 {
		t.Fatal("expected roles block to be extracted")
	}
	return registry, roles[0].Content
}

// TestDictContextWithRegisteredDict verifies that dict literal arguments passed
// as a template/block context are inferred even when the dict helper itself is
// registered in the FuncMap (returning an opaque map[string]interface{}).
func TestDictContextWithRegisteredDict(t *testing.T) {
	registry, rolesContent := dictContextRegistry(t)
	funcMaps := validator.BuildFuncMapRegistry([]ast.FuncMapInfo{
		{Name: "dict", Returns: []ast.ParamInfo{{TypeStr: "map[string]interface{}"}}},
	})
	errs := validator.ValidateTemplateContent(rolesContent, map[string]ast.TemplateVar{}, "views/partials/roles.html", ".", ".", 1, registry, funcMaps)
	if len(errs) != 0 {
		for _, e := range errs {
			t.Errorf("unexpected error: %s (%s)", e.Message, e.Variable)
		}
	}
}

// TestDictContextWithoutRegisteredDict verifies dict literal context inference
// works when dict is only provided by the expression parser.
func TestDictContextWithoutRegisteredDict(t *testing.T) {
	registry, rolesContent := dictContextRegistry(t)
	funcMaps := validator.BuildFuncMapRegistry(nil)
	errs := validator.ValidateTemplateContent(rolesContent, map[string]ast.TemplateVar{}, "views/partials/roles.html", ".", ".", 1, registry, funcMaps)
	if len(errs) != 0 {
		for _, e := range errs {
			t.Errorf("unexpected error: %s (%s)", e.Message, e.Variable)
		}
	}
}

// TestDictContextLocalVariableValue verifies dict values that reference local
// variables and function results still produce inferable keys.
func TestDictContextLocalVariableValue(t *testing.T) {
	content := `{{ define "inner" }}
Value: {{ .v }}
{{ end }}
{{ define "outer" }}
{{ $name := .UserName }}
{{ template "inner" dict "v" $name }}
{{ end }}`
	registry := map[string][]validator.NamedBlockEntry{}
	validator.ExtractNamedTemplatesFromContent(content, "partials.html", "partials.html", registry)
	outer := registry["outer"][0].Content

	errs := validator.ValidateTemplateContent(outer, map[string]ast.TemplateVar{}, "partials.html", ".", ".", 1, registry, validator.BuildFuncMapRegistry(nil))
	if len(errs) != 0 {
		for _, e := range errs {
			t.Errorf("unexpected error: %s (%s)", e.Message, e.Variable)
		}
	}
}
