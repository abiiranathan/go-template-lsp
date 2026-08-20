package validator_test

import (
	"testing"

	"github.com/abiiranathan/go-template-lsp/gotpl-analyzer/ast"
	"github.com/abiiranathan/go-template-lsp/gotpl-analyzer/validator"
)

func TestValidateFunctionCallArgTypes_ConvertibleTypes(t *testing.T) {
	// Setup standard TypeRegistry with a known struct to test struct-vs-scalar boundaries
	typeRegistry := map[string][]ast.FieldInfo{
		"User": {
			{Name: "ID", TypeStr: "int"},
			{Name: "Name", TypeStr: "string"},
			{Name: "Role", TypeStr: "int"},
			{Name: "Permission", TypeStr: "Permission"},
			{Name: "Status", TypeStr: "Status"},
		},
		"models.User": {
			{Name: "ID", TypeStr: "int"},
			{Name: "Name", TypeStr: "string"},
		},
	}

	// Setup template variables
	vars := map[string]ast.TemplateVar{
		"User": {
			Name:    "User",
			TypeStr: "User",
			Fields:  typeRegistry["User"],
		},
		"Role": {
			Name:    "Role",
			TypeStr: "int",
		},
		"Perm": {
			Name:    "Perm",
			TypeStr: "Permission",
		},
		"UserStatus": {
			Name:    "UserStatus",
			TypeStr: "Status",
		},
		"RawString": {
			Name:    "RawString",
			TypeStr: "string",
		},
	}

	// Setup registered template functions
	funcMaps := validator.FuncMapRegistry{
		"hasPermission": {
			Name: "hasPermission",
			Params: []ast.ParamInfo{
				{Name: "p", TypeStr: "Permission"},
			},
		},
		"checkPkgPermission": {
			Name: "checkPkgPermission",
			Params: []ast.ParamInfo{
				{Name: "p", TypeStr: "auth.Permission"},
			},
		},
		"takeInt": {
			Name: "takeInt",
			Params: []ast.ParamInfo{
				{Name: "val", TypeStr: "int"},
			},
		},
		"takeInt64": {
			Name: "takeInt64",
			Params: []ast.ParamInfo{
				{Name: "val", TypeStr: "int64"},
			},
		},
		"renderHTML": {
			Name: "renderHTML",
			Params: []ast.ParamInfo{
				{Name: "h", TypeStr: "template.HTML"},
			},
		},
		"checkStatus": {
			Name: "checkStatus",
			Params: []ast.ParamInfo{
				{Name: "s", TypeStr: "Status"},
			},
		},
		"takeUser": {
			Name: "takeUser",
			Params: []ast.ParamInfo{
				{Name: "u", TypeStr: "User"},
			},
		},
		"multiArg": {
			Name: "multiArg",
			Params: []ast.ParamInfo{
				{Name: "p", TypeStr: "Permission"},
				{Name: "s", TypeStr: "Status"},
			},
		},
	}

	tests := []struct {
		name          string
		expr          string
		expectErrors  bool
		expectedCount int
	}{
		// --- 1. DEFINED NUMERIC TYPES (type Permission int) ---
		{
			name:         "Int literal to Permission parameter",
			expr:         "hasPermission 1",
			expectErrors: false,
		},
		{
			name:         "Int variable to Permission parameter",
			expr:         "hasPermission .Role",
			expectErrors: false,
		},
		{
			name:         "Int field to Permission parameter",
			expr:         "hasPermission .User.Role",
			expectErrors: false,
		},
		{
			name:         "Permission variable to Permission parameter",
			expr:         "hasPermission .Perm",
			expectErrors: false,
		},
		{
			name:         "Permission field to Permission parameter",
			expr:         "hasPermission .User.Permission",
			expectErrors: false,
		},
		{
			name:         "Int literal to Package-qualified auth.Permission",
			expr:         "checkPkgPermission 42",
			expectErrors: false,
		},
		{
			name:         "Permission to standard int parameter (reverse conversion)",
			expr:         "takeInt .Perm",
			expectErrors: false,
		},
		{
			name:         "Int to int64 parameter (numeric cross-promotion)",
			expr:         "takeInt64 100",
			expectErrors: false,
		},
		{
			name:         "Pipelined int to Permission function",
			expr:         ".User.Role | hasPermission",
			expectErrors: false,
		},

		// --- 2. DEFINED STRING & HTML TYPES ---
		{
			name:         "String literal to template.HTML",
			expr:         `renderHTML "<b>Hello</b>"`,
			expectErrors: false,
		},
		{
			name:         "String variable to template.HTML",
			expr:         "renderHTML .RawString",
			expectErrors: false,
		},
		{
			name:         "String literal to custom Status string type",
			expr:         `checkStatus "active"`,
			expectErrors: false,
		},
		{
			name:         "Status variable to Status parameter",
			expr:         "checkStatus .UserStatus",
			expectErrors: false,
		},

		// --- 3. MULTI-ARGUMENT CALLS ---
		{
			name:         "Multi-arg valid with convertible types",
			expr:         `multiArg 1 "active"`,
			expectErrors: false,
		},
		{
			name:         "Multi-arg valid with variable fields",
			expr:         "multiArg .User.Role .UserStatus",
			expectErrors: false,
		},

		// --- 4. INVALID ARGUMENTS (SHOULD STILL FAIL) ---
		{
			name:          "String literal passed to Permission (type mismatch)",
			expr:          `hasPermission "admin"`,
			expectErrors:  true,
			expectedCount: 1,
		},
		{
			name:          "String variable passed to Permission (type mismatch)",
			expr:          "hasPermission .RawString",
			expectErrors:  true,
			expectedCount: 1,
		},
		{
			name:          "Int literal passed to Status (type mismatch)",
			expr:          "checkStatus 123",
			expectErrors:  true,
			expectedCount: 1,
		},
		{
			name:          "Int variable passed to Status (type mismatch)",
			expr:          "checkStatus .Role",
			expectErrors:  true,
			expectedCount: 1,
		},
		{
			name:          "Struct User passed to Permission (struct to scalar mismatch)",
			expr:          "hasPermission .User",
			expectErrors:  true,
			expectedCount: 1,
		},
		{
			name:          "Int passed to Struct User parameter",
			expr:          "takeUser 123",
			expectErrors:  true,
			expectedCount: 1,
		},
		{
			name:          "Multi-arg: first valid, second invalid",
			expr:          "multiArg 1 999",
			expectErrors:  true,
			expectedCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mismatches := validator.ValidateFunctionCallArgTypes(
				tt.expr,
				vars,
				nil,
				nil,
				funcMaps,
				typeRegistry,
			)

			if tt.expectErrors {
				if len(mismatches) == 0 {
					t.Fatalf("Expected type mismatches for expr %q, but got 0", tt.expr)
				}
				if tt.expectedCount > 0 && len(mismatches) != tt.expectedCount {
					t.Fatalf("Expected %d mismatches for expr %q, got %d: %+v", tt.expectedCount, tt.expr, len(mismatches), mismatches)
				}
			} else {
				if len(mismatches) > 0 {
					t.Fatalf("Expected no type mismatches for expr %q, but got %d: %+v", tt.expr, len(mismatches), mismatches)
				}
			}
		})
	}
}

func TestTypesCompatible_DirectUnitMatrix(t *testing.T) {
	typeRegistry := map[string][]ast.FieldInfo{
		"User": {
			{Name: "ID", TypeStr: "int"},
		},
	}

	cases := []struct {
		expected       string
		actual         string
		expectedCompat bool
	}{
		// Basic equality
		{"int", "int", true},
		{"string", "string", true},
		{"bool", "bool", true},

		// Any / Interface
		{"any", "int", true},
		{"interface{}", "string", true},
		{"int", "any", true},
		{"int", "interface{}", true},

		// Pointer unwrapping
		{"*Permission", "int", true},
		{"Permission", "*int", true},
		{"*User", "User", true},
		{"User", "*User", true},

		// Custom scalar types
		{"Permission", "int", true},
		{"Permission", "int64", true},
		{"Permission", "float64", true},
		{"int", "Permission", true},
		{"auth.Permission", "int", true},

		// String-like types
		{"template.HTML", "string", true},
		{"Status", "string", true},
		{"string", "Status", true},
		{"template.URL", "string", true},

		// Struct vs primitive / custom scalar (incompatible)
		{"Permission", "User", false},
		{"User", "Permission", false},
		{"User", "int", false},
		{"User", "string", false},

		// Cross-primitive invalid conversions
		{"Permission", "string", false},
		{"Status", "int", false},
		{"bool", "int", false},
		{"int", "bool", false},
	}

	for _, c := range cases {
		t.Run(c.expected+"_vs_"+c.actual, func(t *testing.T) {
			res := &validator.ExpressionTypeResult{TypeStr: c.actual}
			got := validator.TypesCompatible(c.expected, res, typeRegistry)
			if got != c.expectedCompat {
				t.Errorf("typesCompatible(%q, %q) = %v; want %v", c.expected, c.actual, got, c.expectedCompat)
			}
		})
	}
}
