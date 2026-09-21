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
