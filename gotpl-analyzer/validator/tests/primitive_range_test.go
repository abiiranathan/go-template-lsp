package validator_test

import (
	"testing"

	"github.com/abiiranathan/go-template-lsp/gotpl-analyzer/ast"
	"github.com/abiiranathan/go-template-lsp/gotpl-analyzer/validator"
)

// primitiveRangeVars covers slices/maps whose element type is a concrete
// primitive, plus opaque/indeterminate element types that must stay silent.
var primitiveRangeVars = map[string]ast.TemplateVar{
	"strings": {
		Name:     "strings",
		TypeStr:  "[]string",
		IsSlice:  true,
		ElemType: "string",
	},
	"ints": {
		Name:     "ints",
		TypeStr:  "[]int",
		IsSlice:  true,
		ElemType: "int",
	},
	"roles": {
		Name:     "roles",
		TypeStr:  "map[string]string",
		IsMap:    true,
		KeyType:  "string",
		ElemType: "string",
	},
	"anything": {
		Name:     "anything",
		TypeStr:  "[]any",
		IsSlice:  true,
		ElemType: "any",
	},
	"opaque": {
		Name:     "opaque",
		TypeStr:  "[]handlers.Opaque",
		IsSlice:  true,
		ElemType: "handlers.Opaque",
	},
}

func validatePrimitiveRange(t *testing.T, content string) []validator.ValidationResult {
	t.Helper()
	return validator.ValidateTemplateContent(content, primitiveRangeVars, "range.html", ".", ".", 1, nil)
}

// TestRangeOverStringSliceRejectsFieldAccess is the core regression: ranging
// over a []string yields string elements, so {{ .Name }} must be reported.
func TestRangeOverStringSliceRejectsFieldAccess(t *testing.T) {
	content := `{{ range .strings }}{{ .Name }}{{ end }}`
	errs := validatePrimitiveRange(t, content)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %#v", len(errs), errs)
	}
	if errs[0].Variable != ".Name" {
		t.Fatalf("expected error for .Name, got %q", errs[0].Variable)
	}
}

// TestRangeOverPrimitiveAllowsDot ensures the element itself is still valid.
func TestRangeOverPrimitiveAllowsDot(t *testing.T) {
	content := `{{ range .strings }}{{ . }}{{ end }}`
	if errs := validatePrimitiveRange(t, content); len(errs) != 0 {
		t.Fatalf("expected 0 errors, got %d: %#v", len(errs), errs)
	}
}

// TestRangeOverIntSliceRejectsFieldAccess covers non-string primitives.
func TestRangeOverIntSliceRejectsFieldAccess(t *testing.T) {
	content := `{{ range .ints }}{{ .Value }}{{ end }}`
	errs := validatePrimitiveRange(t, content)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %#v", len(errs), errs)
	}
	if errs[0].Variable != ".Value" {
		t.Fatalf("expected error for .Value, got %q", errs[0].Variable)
	}
}

// TestRangeOverPrimitiveMapRejectsFieldAccess covers map values.
func TestRangeOverPrimitiveMapRejectsFieldAccess(t *testing.T) {
	content := `{{ range .roles }}{{ .Name }}{{ end }}`
	errs := validatePrimitiveRange(t, content)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %#v", len(errs), errs)
	}
	if errs[0].Variable != ".Name" {
		t.Fatalf("expected error for .Name, got %q", errs[0].Variable)
	}
}

// TestNamedRangePrimitiveRejectsFieldAccess covers {{ range $v := .strings }}.
func TestNamedRangePrimitiveRejectsFieldAccess(t *testing.T) {
	content := `{{ range $v := .strings }}{{ $v.Name }}{{ end }}`
	errs := validatePrimitiveRange(t, content)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %#v", len(errs), errs)
	}
	if errs[0].Variable != "$v.Name" {
		t.Fatalf("expected error for $v.Name, got %q", errs[0].Variable)
	}
}

// TestDeepAccessOnPrimitiveIsReported covers .Field.Sub on a primitive element.
func TestDeepAccessOnPrimitiveIsReported(t *testing.T) {
	content := `{{ range .strings }}{{ .Name.Length }}{{ end }}`
	errs := validatePrimitiveRange(t, content)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %#v", len(errs), errs)
	}
	if errs[0].Variable != ".Name.Length" {
		t.Fatalf("expected error for .Name.Length, got %q", errs[0].Variable)
	}
}

// TestRangeOverAnyStaysSilent ensures interface{} elements are not flagged.
func TestRangeOverAnyStaysSilent(t *testing.T) {
	content := `{{ range .anything }}{{ .Name }}{{ end }}`
	if errs := validatePrimitiveRange(t, content); len(errs) != 0 {
		t.Fatalf("expected 0 errors for []any, got %d: %#v", len(errs), errs)
	}
}

// TestRangeOverOpaqueStructStaysSilent ensures types with no field metadata
// (e.g. structs from external packages) are not flagged to avoid false positives.
func TestRangeOverOpaqueStructStaysSilent(t *testing.T) {
	content := `{{ range .opaque }}{{ .Name }}{{ end }}`
	if errs := validatePrimitiveRange(t, content); len(errs) != 0 {
		t.Fatalf("expected 0 errors for opaque struct, got %d: %#v", len(errs), errs)
	}
}
