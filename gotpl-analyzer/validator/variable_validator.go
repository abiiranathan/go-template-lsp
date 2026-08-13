package validator

import (
	"strings"

	"github.com/abiiranathan/go-template-lsp/gotpl-analyzer/ast"
)

// knownTypeMethods maps fully-qualified (or short) type names to the set of
// methods that are callable on that type inside a Go template.
var knownTypeMethods = map[string]map[string]bool{
	// ── Standard library ──────────────────────────────────────────────────
	"time.Time": {
		"Format":      true,
		"String":      true,
		"UTC":         true,
		"Local":       true,
		"Unix":        true,
		"UnixMilli":   true,
		"UnixMicro":   true,
		"UnixNano":    true,
		"IsZero":      true,
		"Before":      true,
		"After":       true,
		"Equal":       true,
		"Add":         true,
		"Sub":         true,
		"Round":       true,
		"Truncate":    true,
		"Date":        true,
		"Clock":       true,
		"Year":        true,
		"Month":       true,
		"Day":         true,
		"Hour":        true,
		"Minute":      true,
		"Second":      true,
		"Nanosecond":  true,
		"Weekday":     true,
		"YearDay":     true,
		"ISOWeek":     true,
		"Zone":        true,
		"In":          true,
		"Location":    true,
		"MarshalText": true,
	},
	"Time": {
		"Format":      true,
		"String":      true,
		"UTC":         true,
		"Local":       true,
		"Unix":        true,
		"UnixMilli":   true,
		"UnixMicro":   true,
		"UnixNano":    true,
		"IsZero":      true,
		"Before":      true,
		"After":       true,
		"Equal":       true,
		"Add":         true,
		"Sub":         true,
		"Round":       true,
		"Truncate":    true,
		"Date":        true,
		"Clock":       true,
		"Year":        true,
		"Month":       true,
		"Day":         true,
		"Hour":        true,
		"Minute":      true,
		"Second":      true,
		"Nanosecond":  true,
		"Weekday":     true,
		"YearDay":     true,
		"ISOWeek":     true,
		"Zone":        true,
		"In":          true,
		"Location":    true,
		"MarshalText": true,
	},

	// ── sql.NullString / NullTime / etc. ──────────────────────────────────
	"NullString":  {"String": true, "Value": true, "Scan": true},
	"NullTime":    {"String": true, "Value": true, "Scan": true, "Format": true},
	"NullBool":    {"Value": true, "Scan": true},
	"NullInt64":   {"Value": true, "Scan": true},
	"NullFloat64": {"Value": true, "Scan": true},

	// ── Domain / custom types ─────────────────────────────────────────────
	"Date": {
		"String": true,
		"Format": true,
		"IsZero": true,
		"Before": true,
		"After":  true,
		"Equal":  true,
		"Time":   true,
	},
}

var universalMethods = map[string]bool{
	"String": true,
	"Error":  true,
}

func typeHasMethod(typeName, methodName string) bool {
	if universalMethods[methodName] {
		return true
	}

	bare := typeName
	for strings.HasPrefix(bare, "*") || strings.HasPrefix(bare, "[]") {
		if strings.HasPrefix(bare, "*") {
			bare = bare[1:]
		} else {
			bare = bare[2:]
		}
	}

	if methods, ok := knownTypeMethods[bare]; ok && methods[methodName] {
		return true
	}

	if idx := strings.LastIndex(bare, "."); idx != -1 {
		short := bare[idx+1:]
		if methods, ok := knownTypeMethods[short]; ok && methods[short] {
			return true
		}
		if methods, ok := knownTypeMethods[short]; ok && methods[methodName] {
			return true
		}
	}

	return false
}

// validateVariableInScope validates a variable access expression in the current scope context.
func validateVariableInScope(varExpr string, scopeStack []ScopeType, varMap map[string]ast.TemplateVar) *ValidationResult {
	varExpr = strings.TrimSpace(varExpr)

	if varExpr == "." || varExpr == "$" {
		return nil
	}

	// ── Local variable access: $var or $var.Field ──────────────────────────
	if strings.HasPrefix(varExpr, "$") && !strings.HasPrefix(varExpr, "$.") {
		localVar, remainder, ok := lookupLocalVar(varExpr, scopeStack)
		if !ok {
			return undefinedVariableError(varExpr)
		}
		if len(remainder) == 0 {
			return nil
		}
		if len(localVar.Fields) == 0 && !localVar.IsMap && !localVar.IsSlice {
			return nil
		}
		return validateNestedFields(varExpr, remainder, localVar.Fields, localVar.TypeStr, localVar.IsMap, localVar.ElemType)
	}

	varExpr = strings.TrimRight(varExpr, ".")
	parts := strings.Split(varExpr, ".")
	if len(parts) < 2 {
		return nil
	}

	isRootAccess := parts[0] == "$"

	// ── Scoped access in nested block ──────────────────────────────────────
	if !isRootAccess && len(scopeStack) > 1 {
		currentScope := scopeStack[len(scopeStack)-1]
		fieldName := parts[1]

		if currentScope.IsMap {
			if len(parts) > 2 {
				return validateNestedFields(varExpr, parts[2:], nil, currentScope.ElemType, false, "")
			}
			return nil
		}

		var foundField *ast.FieldInfo
		for _, f := range currentScope.Fields {
			if f.Name == fieldName {
				fCopy := f
				foundField = &fCopy
				break
			}
		}

		if foundField != nil {
			if len(parts) > 2 {
				return validateNestedFields(varExpr, parts[2:], foundField.Fields, foundField.TypeStr, foundField.IsMap, foundField.ElemType)
			}
			return nil
		}

		// Fallback for root-level scopes (e.g. inside `if` blocks at root level):
		// Check varMap and rootScope directly if currentScope is the root.
		if currentScope.IsRoot {
			if v, ok := varMap[fieldName]; ok {
				if len(parts) > 2 {
					return validateNestedFields(varExpr, parts[2:], v.Fields, v.TypeStr, v.IsMap, v.ElemType)
				}
				return nil
			}
			if len(scopeStack) > 0 {
				for _, f := range scopeStack[0].Fields {
					if f.Name == fieldName {
						if len(parts) > 2 {
							return validateNestedFields(varExpr, parts[2:], f.Fields, f.TypeStr, f.IsMap, f.ElemType)
						}
						return nil
					}
				}
			}
		}

		if len(currentScope.Fields) == 0 {
			return nil
		}

		return undefinedVariableError(varExpr)
	}

	// ── Root variable access ───────────────────────────────────────────────
	if len(parts) == 2 {
		rootVar := parts[1]

		if len(scopeStack) > 0 {
			rootScope := scopeStack[0]
			for _, f := range rootScope.Fields {
				if f.Name == rootVar {
					return nil
				}
			}
		}

		if _, ok := varMap[rootVar]; ok {
			return nil
		}

		if len(scopeStack) > 0 && len(scopeStack[0].Fields) == 0 && len(varMap) == 0 {
			return nil
		}

		if len(scopeStack) == 0 && len(varMap) == 0 {
			return nil
		}

		return undefinedVariableError(varExpr)
	}

	// ── Nested access: .Var.Field.SubField ─────────────────────────────────
	rootVar := parts[1]

	var rootVarInfo *ast.TemplateVar
	if v, ok := varMap[rootVar]; ok {
		rootVarInfo = &v
	} else if len(scopeStack) > 0 {
		rootScope := scopeStack[0]
		for _, f := range rootScope.Fields {
			if f.Name == rootVar {
				if f.IsMap && len(parts) == 3 {
					return nil
				}
				return validateNestedFields(varExpr, parts[2:], f.Fields, f.TypeStr, f.IsMap, f.ElemType)
			}
		}
		if len(rootScope.Fields) == 0 && len(varMap) == 0 {
			return nil
		}
		return undefinedVariableError(varExpr)
	}

	if rootVarInfo != nil {
		if rootVarInfo.IsMap && len(parts) == 3 {
			return nil
		}
		return validateNestedFields(varExpr, parts[2:], rootVarInfo.Fields, rootVarInfo.TypeStr, rootVarInfo.IsMap, rootVarInfo.ElemType)
	}

	return undefinedVariableError(varExpr)
}

func validateNestedFields(fullExpr string, fieldParts []string, fields []ast.FieldInfo, parentTypeName string, isMap bool, elemType string) *ValidationResult {
	currentFields := fields
	parentType := parentTypeName
	currentIsMap := isMap
	currentElemType := elemType

	// Traverse each field in the path
	for _, fieldName := range fieldParts {
		if currentIsMap {
			// ── Map key access ─────────────────────────────────────────────
			// Any key is valid for map access.
			// Parse element type to determine if further nesting is valid.

			baseType := currentElemType
			// Unwrap pointer types
			for strings.HasPrefix(baseType, "*") {
				baseType = baseType[1:]
			}

			newIsMap := false
			newElemType := ""

			if strings.HasPrefix(baseType, "map[") {
				// Nested map: parse map[Key]Value
				// Use bracket counting to handle complex key types like map[string]
				depth := 0
				splitIdx := -1

				// Start after "map[" (index 3)
				for i := 3; i < len(baseType); i++ {
					if baseType[i] == '[' {
						depth++
					} else if baseType[i] == ']' {
						depth--
						if depth == 0 {
							splitIdx = i
							break
						}
					}
				}

				if splitIdx != -1 {
					valType := baseType[splitIdx+1:]
					newIsMap = true
					newElemType = strings.TrimSpace(valType)
				}
			} else if strings.HasPrefix(baseType, "[]") {
				// Slice: element type is after []
				newElemType = baseType[2:]
			}

			currentIsMap = newIsMap
			if newElemType != "" {
				currentElemType = newElemType
			} else {
				// Basic type or struct: use element type as parent type
				parentType = currentElemType
			}

			continue
		}

		// ── Struct field access ────────────────────────────────────────────
		// Field must exist in Fields slice, OR be a recognised method on the
		// current parent type.

		found := false
		var nextFields []ast.FieldInfo
		var nextIsMap bool
		var nextElemType string

		for _, f := range currentFields {
			if f.Name == fieldName {
				found = true
				nextFields = f.Fields
				parentType = f.TypeStr
				nextIsMap = f.IsMap
				nextElemType = f.ElemType
				break
			}
		}

		if !found {
			// ── Method resolution ──────────────────────────────────────────
			if typeHasMethod(parentType, fieldName) {
				// Method is valid; the result type is opaque — stop validation.
				return nil
			}

			if len(currentFields) == 0 {
				return nil
			}

			return undefinedVariableError(fullExpr)
		}

		// Move to next level in hierarchy
		currentFields = nextFields
		currentIsMap = nextIsMap
		currentElemType = nextElemType
	}

	return nil
}

func undefinedVariableError(varExpr string) *ValidationResult {
	return &ValidationResult{
		Variable: varExpr,
		Message:  `Template variable "` + varExpr + `" is not defined in the current scope`,
		Severity: "error",
	}
}

// validateContextArg checks whether a template call context expression
// resolves in the current scope.
//
// Used to validate that context arguments in {{template "name" .Context}}
// actually exist before recursively validating the nested template.
//
// Returns true if valid, false if undefined.
//
// Thread-safety: Read-only operations, safe for concurrent calls.
func validateContextArg(
	contextArg string,
	scopeStack []ScopeType,
	varMap map[string]ast.TemplateVar,
	funcMaps FuncMapRegistry,
) *ValidationResult {
	// Special cases always valid
	if contextArg == "" || contextArg == "." || contextArg == "$" {
		return nil
	}

	inferred := InferExpressionType(contextArg, varMap, scopeStack, nil, funcMaps, nil)
	if inferred != nil {
		return nil
	}

	// Validate using standard validation logic
	return validateVariableInScope(contextArg, scopeStack, varMap)
}
