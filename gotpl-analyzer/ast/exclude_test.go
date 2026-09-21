package ast

import (
	"os"
	"path/filepath"
	"testing"
)

func writeExcludeTestModule(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte("module example.com/excludetest\ngo 1.21\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmpDir, "pdf"), 0755); err != nil {
		t.Fatal(err)
	}
	pdfSrc := `package pdf

type Doc struct{}

func (d *Doc) Render(name string, data map[string]any) error { return nil }
`
	if err := os.WriteFile(filepath.Join(tmpDir, "pdf", "pdf.go"), []byte(pdfSrc), 0644); err != nil {
		t.Fatal(err)
	}
	mainSrc := `package main

import "example.com/excludetest/pdf"

type WebCtx struct{}

func (c *WebCtx) Render(name string, data map[string]any) error { return nil }

func webHandler(c *WebCtx) {
	c.Render("index.html", map[string]any{"Title": "hi"})
}

func pdfHandler(d *pdf.Doc) {
	d.Render("output.pdf", map[string]any{"Title": "hi"})
}
`
	if err := os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte(mainSrc), 0644); err != nil {
		t.Fatal(err)
	}
	return tmpDir
}

func collectTemplates(renderCalls []RenderCall) map[string]bool {
	out := make(map[string]bool)
	for _, rc := range renderCalls {
		out[rc.Template] = true
	}
	return out
}

func TestExcludePackagesIgnoresNonTemplateRender(t *testing.T) {
	dir := writeExcludeTestModule(t)

	// Baseline: both Render methods detected (output.pdf is the false positive).
	base := DefaultConfig
	result := AnalyzeDir(dir, "", &base)
	found := collectTemplates(result.RenderCalls)
	if !found["index.html"] {
		t.Errorf("expected index.html render call, got %v", found)
	}
	if !found["output.pdf"] {
		t.Errorf("expected baseline to include output.pdf false positive, got %v", found)
	}

	// Excluding by plain package name drops only the pdf call.
	cfg := DefaultConfig
	cfg.ExcludePackages = []string{"pdf"}
	result = AnalyzeDir(dir, "", &cfg)
	found = collectTemplates(result.RenderCalls)
	if !found["index.html"] {
		t.Errorf("expected index.html to survive exclusion, got %v", found)
	}
	if found["output.pdf"] {
		t.Errorf("expected output.pdf to be excluded by package name, got %v", found)
	}

	// Excluding by full import path behaves the same.
	cfg = DefaultConfig
	cfg.ExcludePackages = []string{"example.com/excludetest/pdf"}
	result = AnalyzeDir(dir, "", &cfg)
	found = collectTemplates(result.RenderCalls)
	if !found["index.html"] {
		t.Errorf("expected index.html to survive full-path exclusion, got %v", found)
	}
	if found["output.pdf"] {
		t.Errorf("expected output.pdf to be excluded by full path, got %v", found)
	}
}

func TestExcludePackagesIgnoresCallerDirectory(t *testing.T) {
	// A call to web.Ctx.Render located in tui/ must be dropped by excluding
	// the caller directory, even though the callee package (web) is kept.
	tmpDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte("module example.com/callertest\ngo 1.21\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmpDir, "web"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmpDir, "tui"), 0755); err != nil {
		t.Fatal(err)
	}
	webSrc := `package web

type Ctx struct{}

func (c *Ctx) Render(name string, data map[string]any) error { return nil }
`
	if err := os.WriteFile(filepath.Join(tmpDir, "web", "web.go"), []byte(webSrc), 0644); err != nil {
		t.Fatal(err)
	}
	tuiSrc := `package tui

import "example.com/callertest/web"

func handler(c *web.Ctx) {
	c.Render("tui.html", map[string]any{"Title": "hi"})
}
`
	if err := os.WriteFile(filepath.Join(tmpDir, "tui", "tui.go"), []byte(tuiSrc), 0644); err != nil {
		t.Fatal(err)
	}
	rootSrc := `package main

import "example.com/callertest/web"

func webHandler(c *web.Ctx) {
	c.Render("index.html", map[string]any{"Title": "hi"})
}
`
	if err := os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte(rootSrc), 0644); err != nil {
		t.Fatal(err)
	}

	base := DefaultConfig
	found := collectTemplates(AnalyzeDir(tmpDir, "", &base).RenderCalls)
	if !found["index.html"] || !found["tui.html"] {
		t.Fatalf("expected baseline to include index.html and tui.html, got %v", found)
	}

	for _, entry := range []string{"tui", "example.com/callertest/tui"} {
		cfg := DefaultConfig
		cfg.ExcludePackages = []string{entry}
		found = collectTemplates(AnalyzeDir(tmpDir, "", &cfg).RenderCalls)
		if !found["index.html"] {
			t.Errorf("entry %q: expected index.html to survive, got %v", entry, found)
		}
		if found["tui.html"] {
			t.Errorf("entry %q: expected tui.html to be excluded by caller dir, got %v", entry, found)
		}
	}
}

func TestMatchesExcludedCaller(t *testing.T) {
	cases := []struct {
		relFile, exclude string
		want             bool
	}{
		{"internal/tui/app.go", "internal/tui", true},
		{"internal/tui/app.go", "tui", true},
		{"internal/tui/app.go", "github.com/abiiranathan/eclinichms/internal/tui", true},
		{"internal/tui/app.go", "internal", true},
		{"internal/tui/app.go", "internal/tui/app.go", true},
		{"internal/tui/app.go", "other", false},
		{"internal/tui/app.go", "tu", false}, // no partial-segment match
		{"main.go", "main.go", true},
		{"main.go", "other", false},
	}
	for _, tc := range cases {
		if got := matchesExcludedCaller(tc.relFile, []string{tc.exclude}); got != tc.want {
			t.Errorf("matchesExcludedCaller(%q,[%q]) = %v, want %v", tc.relFile, tc.exclude, got, tc.want)
		}
	}
}

func TestMatchesExcludedPackage(t *testing.T) {
	cases := []struct {
		path, name, exclude string
		want                bool
	}{
		{"github.com/foo/pdf", "pdf", "pdf", true},
		{"github.com/foo/pdf", "pdf", "foo/pdf", true},
		{"github.com/foo/pdf", "pdf", "github.com/foo/pdf", true},
		{"github.com/foo/pdf", "pdf", "other", false},
		{"github.com/foo/pdf", "pdf", "pd", false}, // no partial-name match
		{"", "pdf", "pdf", true},                   // AST-only qualifier fallback
		{"", "web", "pdf", false},
	}
	for _, tc := range cases {
		if got := matchesExcludedPackage(tc.path, tc.name, []string{tc.exclude}); got != tc.want {
			t.Errorf("matchesExcludedPackage(%q,%q,[%q]) = %v, want %v", tc.path, tc.name, tc.exclude, got, tc.want)
		}
	}
}
