package ast

import (
	"strings"
	"testing"
)

// TestExternalMethodDocs verifies that doc comments for methods of types that
// live outside the analyzed project (e.g. stdlib time.Time) are resolved from
// the external source file, not just from the project's struct index.
func TestExternalMethodDocs(t *testing.T) {
	dir := t.TempDir()
	writeTestModule(t, dir, `package main

import (
	"time"
)

func Render(w http.ResponseWriter, template string, data interface{}) {}

func main() {
	Render(nil, "post.html", map[string]interface{}{
		"CreatedAt": time.Time{},
	})
}
`)

	result := AnalyzeDir(dir, "", &DefaultConfig)
	result.BuildTypeRegistry()

	if len(result.RenderCalls) == 0 {
		t.Fatal("no render calls found")
	}

	var postVar *TemplateVar
	for i := range result.RenderCalls[0].Vars {
		if result.RenderCalls[0].Vars[i].Name == "CreatedAt" {
			postVar = &result.RenderCalls[0].Vars[i]
			break
		}
	}
	if postVar == nil {
		t.Fatalf("CreatedAt var not found in %#v", result.RenderCalls[0].Vars)
	}
	if postVar.TypeStr != "time.Time" {
		t.Fatalf("expected CreatedAt type time.Time, got %s", postVar.TypeStr)
	}

	formatDoc := findMethodDoc(t, postVar.Fields, "Format")
	if formatDoc == "" {
		t.Fatal("Format method has no doc; external method doc extraction failed")
	}
	if !strings.Contains(formatDoc, "textual representation of the time value") {
		t.Errorf("unexpected Format doc: %q", formatDoc)
	}

	addDoc := findMethodDoc(t, postVar.Fields, "Add")
	if !strings.Contains(addDoc, "Add returns the time t+d") {
		t.Errorf("unexpected Add doc: %q", addDoc)
	}

	// The external type's own doc comment should also be resolved.
	if postVar.Doc == "" {
		t.Error("time.Time type doc missing; external type doc extraction failed")
	}
	if !strings.Contains(postVar.Doc, "A Time represents an instant in time") {
		t.Errorf("unexpected time.Time doc: %q", postVar.Doc)
	}
}

// findMethodDoc returns the doc comment of the method with the given name.
func findMethodDoc(t *testing.T, fields []FieldInfo, name string) string {
	t.Helper()
	for _, f := range fields {
		if f.TypeStr == "method" && f.Name == name {
			return f.Doc
		}
	}
	return ""
}

// TestExternalFuncMapDocs verifies that doc comments for functions living
// outside the analyzed project (e.g. stdlib strings.ToUpper) are resolved when
// registered in a template.FuncMap.
func TestExternalFuncMapDocs(t *testing.T) {
	dir := t.TempDir()
	writeTestModule(t, dir, `package main

import (
	"strings"
	"text/template"
)

var GlobalFuncMap = template.FuncMap{
	"toUpper": strings.ToUpper,
	"title":   strings.Title,
}

func main() {
	_ = GlobalFuncMap
}
`)

	result := AnalyzeDir(dir, "", &DefaultConfig)

	docs := make(map[string]string)
	for _, fm := range result.FuncMaps {
		docs[fm.Name] = fm.Doc
	}

	if doc := docs["toUpper"]; !strings.Contains(doc, "returns s with all Unicode letters mapped to their upper case") {
		t.Errorf("toUpper doc missing or wrong: %q", doc)
	}
	if doc := docs["title"]; doc == "" {
		t.Error("title doc missing")
	}
}
