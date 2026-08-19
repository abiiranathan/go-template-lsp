package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abiiranathan/go-template-lsp/gotpl-analyzer/validator"
)

// TestDaemonFooterRegressionRealRoot runs the daemon against the checked-in
// sample project (real handler.go with rex.Map, pointer visit, etc.) to
// reproduce the .Patient.Name false positive.
func TestDaemonFooterRegressionRealRoot(t *testing.T) {
	dir := "/home/nabiizy/Code/go/go-template-lsp"

	d := &analyzerDaemon{templateOverlays: make(map[string]string)}

	out1, err := d.analyze(daemonAnalyzeParams{Dir: dir, TemplateRoot: "sample/templates", Validate: true})
	if err != nil {
		t.Fatal(err)
	}

	out2, err := d.reanalyzeTemplates(daemonAnalyzeParams{Dir: dir, TemplateRoot: "sample/templates", Validate: true})
	if err != nil {
		t.Fatal(err)
	}

	hasPatient := func(errs []validator.ValidationResult) bool {
		for _, e := range errs {
			if strings.Contains(e.Variable, ".Patient.Name") || strings.Contains(e.Message, ".Patient.Name") {
				return true
			}
		}
		return false
	}
	if hasPatient(out1.ValidationErrors) {
		t.Errorf("cold analysis produced false positive .Patient.Name")
	}
	if hasPatient(out2.ValidationErrors) {
		t.Errorf("reanalyze produced false positive .Patient.Name")
	}

	snap := d.state.Load()
	if vars, ok := snap.renderVarsByTemplate["views/partials/footer.html"]; ok {
		for _, v := range vars {
			if v.Name == "." && len(v.Fields) == 0 {
				t.Errorf("footer.html '.' var lost its fields in renderVarsByTemplate (type=%s, fields=%d)", v.TypeStr, len(v.Fields))
			}
		}
	} else {
		t.Errorf("footer.html missing from renderVarsByTemplate")
	}

	// Live validation of treatment-chart.html and footer.html
	for _, rel := range []string{
		"sample/templates/views/inpatient/treatment-chart.html",
		"sample/templates/views/partials/footer.html",
	} {
		abs := filepath.Join(dir, rel)
		content, err := os.ReadFile(abs)
		if err != nil {
			t.Fatal(err)
		}
		vt, err := d.validateTemplate(daemonValidateTemplateParams{AbsolutePath: abs, Content: string(content)})
		if err != nil {
			t.Fatal(err)
		}
		if hasPatient(vt.ValidationErrors) {
			t.Errorf("live validateTemplate produced false positive .Patient.Name for %s", rel)
		}
	}
}
