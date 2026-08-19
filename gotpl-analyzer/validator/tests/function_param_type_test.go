package validator_test

import (
	"testing"

	"github.com/abiiranathan/go-template-lsp/gotpl-analyzer/ast"
	"github.com/abiiranathan/go-template-lsp/gotpl-analyzer/validator"
)

func buildFormatFuncMap() ast.FuncMapInfo {
	return ast.FuncMapInfo{
		Name: "formatName",
		Params: []ast.ParamInfo{
			{Name: "name", TypeStr: "string"},
			{Name: "age", TypeStr: "int"},
		},
	}
}

func buildConcatFuncMap() ast.FuncMapInfo {
	return ast.FuncMapInfo{
		Name:   "concat",
		Params: []ast.ParamInfo{{Name: "items", TypeStr: "...string"}},
	}
}

func TestFunctionParamTypeCorrectArgsPass(t *testing.T) {
	content := `{{ formatName .Name 30 }}`
	funcMaps := validator.BuildFuncMapRegistry([]ast.FuncMapInfo{buildFormatFuncMap()})
	vars := map[string]ast.TemplateVar{
		".": {Name: ".", TypeStr: "*User", Fields: []ast.FieldInfo{{Name: "Name", TypeStr: "string"}}},
	}
	errList := validator.ValidateTemplateContent(content, vars, "types.html", ".", ".", 1, nil, funcMaps)
	if len(errList) != 0 {
		t.Fatalf("expected no errors for correct arg types, got %#v", errList)
	}
}

func TestFunctionParamTypeStringVsIntReported(t *testing.T) {
	content := `{{ formatName 42 30 }}`
	funcMaps := validator.BuildFuncMapRegistry([]ast.FuncMapInfo{buildFormatFuncMap()})
	errList := validator.ValidateTemplateContent(content, map[string]ast.TemplateVar{}, "types.html", ".", ".", 1, nil, funcMaps)
	if len(errList) != 1 {
		t.Fatalf("expected 1 type mismatch error, got %d: %#v", len(errList), errList)
	}
	if errList[0].Variable != "formatName" {
		t.Fatalf("expected error on formatName, got %q", errList[0].Variable)
	}
}

func TestFunctionParamTypeFieldMismatchReported(t *testing.T) {
	content := `{{ formatName .Age 30 }}`
	funcMaps := validator.BuildFuncMapRegistry([]ast.FuncMapInfo{buildFormatFuncMap()})
	vars := map[string]ast.TemplateVar{
		".": {Name: ".", TypeStr: "*User", Fields: []ast.FieldInfo{
			{Name: "Age", TypeStr: "int"},
		}},
	}
	errList := validator.ValidateTemplateContent(content, vars, "types.html", ".", ".", 1, nil, funcMaps)
	if len(errList) != 1 {
		t.Fatalf("expected 1 type mismatch error, got %d: %#v", len(errList), errList)
	}
}

func TestFunctionParamTypeLocalVarReported(t *testing.T) {
	content := `
		{{ $name := .Age }}
		{{ formatName $name 30 }}
	`
	funcMaps := validator.BuildFuncMapRegistry([]ast.FuncMapInfo{buildFormatFuncMap()})
	vars := map[string]ast.TemplateVar{
		".": {Name: ".", TypeStr: "*User", Fields: []ast.FieldInfo{
			{Name: "Age", TypeStr: "int"},
		}},
	}
	errList := validator.ValidateTemplateContent(content, vars, "types.html", ".", ".", 1, nil, funcMaps)
	if len(errList) != 1 {
		t.Fatalf("expected 1 type mismatch error for local var arg, got %d: %#v", len(errList), errList)
	}
}

func TestFunctionParamTypeVariadicCorrectArgsPass(t *testing.T) {
	content := `{{ concat "a" "b" "c" }}`
	funcMaps := validator.BuildFuncMapRegistry([]ast.FuncMapInfo{buildConcatFuncMap()})
	errList := validator.ValidateTemplateContent(content, map[string]ast.TemplateVar{}, "types.html", ".", ".", 1, nil, funcMaps)
	if len(errList) != 0 {
		t.Fatalf("expected no errors for variadic string args, got %#v", errList)
	}
}

func TestFunctionParamTypeVariadicMismatchReported(t *testing.T) {
	content := `{{ concat "a" 42 }}`
	funcMaps := validator.BuildFuncMapRegistry([]ast.FuncMapInfo{buildConcatFuncMap()})
	errList := validator.ValidateTemplateContent(content, map[string]ast.TemplateVar{}, "types.html", ".", ".", 1, nil, funcMaps)
	if len(errList) != 1 {
		t.Fatalf("expected 1 type mismatch error for variadic arg, got %d: %#v", len(errList), errList)
	}
}

func TestFunctionParamTypeNestedCallReported(t *testing.T) {
	content := `{{ formatName (concat 42) 30 }}`
	funcMaps := validator.BuildFuncMapRegistry([]ast.FuncMapInfo{
		buildFormatFuncMap(),
		buildConcatFuncMap(),
	})
	errList := validator.ValidateTemplateContent(content, map[string]ast.TemplateVar{}, "types.html", ".", ".", 1, nil, funcMaps)
	if len(errList) != 1 {
		t.Fatalf("expected 1 type mismatch error for nested call, got %d: %#v", len(errList), errList)
	}
	if errList[0].Variable != "concat" {
		t.Fatalf("expected nested concat mismatch, got %q", errList[0].Variable)
	}
}

func TestFunctionParamTypeInsideWithAndRange(t *testing.T) {
	content := `
		{{ with .User }}
			{{ formatName .Name 30 }}
		{{ end }}
		{{ range .Users }}
			{{ formatName 42 30 }}
		{{ end }}
	`
	funcMaps := validator.BuildFuncMapRegistry([]ast.FuncMapInfo{buildFormatFuncMap()})
	vars := map[string]ast.TemplateVar{
		".": {Name: ".", TypeStr: "context", Fields: []ast.FieldInfo{
			{Name: "User", TypeStr: "*User", Fields: []ast.FieldInfo{{Name: "Name", TypeStr: "string"}}},
			{Name: "Users", TypeStr: "[]User", IsSlice: true, ElemType: "User"},
		}},
	}
	errList := validator.ValidateTemplateContent(content, vars, "types.html", ".", ".", 1, nil, funcMaps)
	if len(errList) != 1 {
		t.Fatalf("expected 1 error (inside range), got %d: %#v", len(errList), errList)
	}
	if errList[0].Line != 6 {
		t.Fatalf("expected error on line 6, got %d", errList[0].Line)
	}
}

func TestFunctionParamTypePipedValueReported(t *testing.T) {
	content := `{{ 42 | formatName }}`
	funcMaps := validator.BuildFuncMapRegistry([]ast.FuncMapInfo{buildFormatFuncMap()})
	errList := validator.ValidateTemplateContent(content, map[string]ast.TemplateVar{}, "types.html", ".", ".", 1, nil, funcMaps)
	if len(errList) != 1 {
		t.Fatalf("expected 1 error for piped value type mismatch, got %d: %#v", len(errList), errList)
	}
}

func TestFunctionParamTypePipedValueCorrectPasses(t *testing.T) {
	funcMaps := validator.BuildFuncMapRegistry([]ast.FuncMapInfo{
		{Name: "greet", Params: []ast.ParamInfo{{Name: "name", TypeStr: "string"}}},
	})
	content := `{{ "Jane" | greet }}`
	errList := validator.ValidateTemplateContent(content, map[string]ast.TemplateVar{}, "types.html", ".", ".", 1, nil, funcMaps)
	if len(errList) != 0 {
		t.Fatalf("expected no errors for piped string arg, got %#v", errList)
	}
}

func TestFunctionParamTypePointerArgAccepted(t *testing.T) {
	content := `{{ greetUser .User }}`
	funcMaps := validator.BuildFuncMapRegistry([]ast.FuncMapInfo{
		{Name: "greetUser", Params: []ast.ParamInfo{{Name: "u", TypeStr: "User"}}},
	})
	vars := map[string]ast.TemplateVar{
		".": {Name: ".", TypeStr: "context", Fields: []ast.FieldInfo{{Name: "User", TypeStr: "*User"}}},
	}
	errList := validator.ValidateTemplateContent(content, vars, "types.html", ".", ".", 1, nil, funcMaps)
	if len(errList) != 0 {
		t.Fatalf("expected pointer arg to be accepted for value param, got %#v", errList)
	}
}

func TestFunctionParamTypeInsideElseWith(t *testing.T) {
	content := `
		{{ if .Flag }}
			x
		{{ else with formatName 42 30 }}
			{{ . }}
		{{ end }}
	`
	funcMaps := validator.BuildFuncMapRegistry([]ast.FuncMapInfo{buildFormatFuncMap()})
	errList := validator.ValidateTemplateContent(content, map[string]ast.TemplateVar{}, "types.html", ".", ".", 1, nil, funcMaps)
	if len(errList) != 1 {
		t.Fatalf("expected 1 error for else with arg type mismatch, got %d: %#v", len(errList), errList)
	}
}

func TestFunctionParamTypeUnknownArgsAreSkipped(t *testing.T) {
	content := `{{ formatName .Missing 30 }}`
	funcMaps := validator.BuildFuncMapRegistry([]ast.FuncMapInfo{buildFormatFuncMap()})
	errList := validator.ValidateTemplateContent(content, map[string]ast.TemplateVar{}, "types.html", ".", ".", 1, nil, funcMaps)
	for _, err := range errList {
		if err.Variable == "formatName" {
			t.Fatalf("expected no type error for unknown arg type, got %#v", err)
		}
	}
}
