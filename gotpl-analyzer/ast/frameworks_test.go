package ast

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFrameworksCompatibility(t *testing.T) {
	tmpDir := t.TempDir()

	src := `package main

import (
	"html/template"
	"io"
	"net/http"
)

type User struct {
	Name  string
	Email string
}

type PageData struct {
	Title string
	User  User
}

// 1. Standard Library
func stdlibHandler(w io.Writer, tmpl *template.Template) {
	u := User{Name: "Alice", Email: "alice@example.com"}
	tmpl.ExecuteTemplate(w, "stdlib.html", u)
}

// 2. Gin Pattern: c.HTML(code, "template.html", data) & c.Set
type GinContext struct{}
func (c *GinContext) HTML(code int, name string, obj any) {}
func (c *GinContext) Set(key string, val any) {}

func ginHandler(c *GinContext) {
	c.Set("globalUser", User{Name: "Admin"})
	c.HTML(http.StatusOK, "gin.html", map[string]any{
		"Title": "Gin Page",
	})
}

// 3. Echo Pattern: c.Render(code, "template.html", data)
type EchoContext struct{}
func (c *EchoContext) Render(code int, name string, data any) error { return nil }
func (c *EchoContext) Set(key string, val any) {}

func echoHandler(c *EchoContext) {
	c.Render(http.StatusOK, "echo.html", PageData{
		Title: "Echo Page",
		User:  User{Name: "Bob"},
	})
}

// 4. Fiber Pattern: c.Render("template.html", data) & c.Locals
type FiberCtx struct{}
func (c *FiberCtx) Render(name string, bind any, layouts ...string) error { return nil }
func (c *FiberCtx) Locals(key string, val any) {}

func fiberHandler(c *FiberCtx) {
	c.Locals("fiberUser", User{Name: "Charlie"})
	c.Render("fiber.html", map[string]any{
		"Title": "Fiber Page",
	})
}
`

	if err := os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte("module example.com/test\ngo 1.21\n"), 0644); err != nil {
		t.Fatal(err)
	}

	result := AnalyzeDir(tmpDir, "", &DefaultConfig)
	if len(result.Errors) > 0 {
		t.Fatalf("unexpected analysis errors: %v", result.Errors)
	}

	expectedTemplates := map[string]bool{
		"stdlib.html": false,
		"gin.html":    false,
		"echo.html":   false,
		"fiber.html":  false,
	}

	for _, rc := range result.RenderCalls {
		if _, ok := expectedTemplates[rc.Template]; ok {
			expectedTemplates[rc.Template] = true
		}
	}

	for tpl, found := range expectedTemplates {
		if !found {
			t.Errorf("expected render call for template %q was not detected", tpl)
		}
	}
}
