package main

import (
	"os"
	"testing"
)

// BenchmarkDaemonAnalyzeLarge analyzes a pre-generated large workspace.
// Set GOTPL_BENCH_DIR to the workspace root (see scripts or generate one
// with many Go packages and templates). Skipped when unset so normal
// `go test ./...` runs are unaffected.
func BenchmarkDaemonAnalyzeLarge(b *testing.B) {
	dir := os.Getenv("GOTPL_BENCH_DIR")
	if dir == "" {
		b.Skip("GOTPL_BENCH_DIR not set")
	}

	d := &analyzerDaemon{
		templateOverlays: make(map[string]string),
	}

	params := daemonAnalyzeParams{
		Dir:          dir,
		TemplateRoot: "templates",
		Validate:     true,
	}

	b.ResetTimer()
	for b.Loop() {
		_, err := d.analyze(params)
		if err != nil {
			b.Fatalf("analyze error: %v", err)
		}
	}
}
