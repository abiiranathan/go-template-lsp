package validator_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/abiiranathan/go-template-lsp/gotpl-analyzer/ast"
	"github.com/abiiranathan/go-template-lsp/gotpl-analyzer/validator"
)

func writeTempModule(t *testing.T, tmpls map[string]string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "footer-reg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	mod := `module repro

go 1.21
`
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod), 0o644); err != nil {
		t.Fatal(err)
	}

	for rel, content := range tmpls {
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestFooterPartialRegression reproduces the regression where a cross-file partial
// invoked via {{ template "views/partials/footer.html" .visit }} reports
// ".Patient.Name is not defined" even though .visit has a Patient field.
func TestFooterPartialRegression(t *testing.T) {
	dir := writeTempModule(t, map[string]string{
		"handler.go": `package repro

import (
	"html/template"
)

type Patient struct {
	Name string
}

type Visit struct {
	Patient Patient
}

func Render() {
	visit := &Visit{}
	data := map[string]any{
		"visit": visit,
	}
	t := template.New("")
	t = template.Must(t.Parse(` + "`" + `{{ define "views/inpatient/treatment-chart.html" }}x{{ end }}` + "`" + `))
	_ = t.ExecuteTemplate(nil, "views/inpatient/treatment-chart.html", data)
}
`,
		"templates/views/inpatient/treatment-chart.html": `
			{{ template "views/partials/footer.html" .visit }}
		`,
		"templates/views/partials/footer.html": `
			<h1>{{ .Patient.Name }}</h1>
		`,
	})

	result := ast.AnalyzeDir(dir, "", &ast.DefaultConfig)
	t.Logf("analysis errors: %d", len(result.Errors))
	for _, e := range result.Errors {
		t.Logf("  analyze err: %v", e)
	}
	t.Logf("render calls: %d", len(result.RenderCalls))
	for _, rc := range result.RenderCalls {
		t.Logf("render: template=%s vars=%d", rc.Template, len(rc.Vars))
		for _, v := range rc.Vars {
			t.Logf("  var %s type=%s fields=%d isSlice=%v isMap=%v", v.Name, v.TypeStr, len(v.Fields), v.IsSlice, v.IsMap)
		}
	}

	errs, _, _ := validator.ValidateTemplates(result.RenderCalls, result.FuncMaps, dir, "templates")
	for _, e := range errs {
		t.Logf("ERR: %s | var=%s | tpl=%s", e.Message, e.Variable, e.Template)
	}
	if len(errs) != 0 {
		t.Errorf("expected 0 validation errors, got %d", len(errs))
	}
}

// TestFooterPropagatedIndex inspects what BuildPropagatedRenderVarIndex produces
// for the footer partial when invoked via {{ template "footer.html" .visit }}.
func TestFooterPropagatedIndex(t *testing.T) {
	dir := writeTempModule(t, map[string]string{
		"handler.go": `package repro

import (
	"html/template"
)

type Patient struct {
	Name string
}

type Visit struct {
	Patient Patient
}

func Render() {
	data := map[string]any{
		"visit": Visit{},
	}
	t := template.New("")
	t = template.Must(t.Parse(` + "`" + `{{ define "views/inpatient/treatment-chart.html" }}x{{ end }}` + "`" + `))
	_ = t.ExecuteTemplate(nil, "views/inpatient/treatment-chart.html", data)
}
`,
		"templates/views/inpatient/treatment-chart.html": `
			{{ template "views/partials/footer.html" .visit }}
		`,
		"templates/views/partials/footer.html": `
			<h1>{{ .Patient.Name }}</h1>
		`,
	})

	result := ast.AnalyzeDir(dir, "", &ast.DefaultConfig)
	store := validator.LoadTemplateStore(dir, "templates")
	namedBlocks, _ := validator.ParseAllNamedTemplatesFromStore(store, dir, "templates")
	funcMapReg := validator.BuildFuncMapRegistry(result.FuncMaps)

	idx := validator.BuildPropagatedRenderVarIndex(result.RenderCalls, namedBlocks, dir, "templates", funcMapReg, store)
	for tpl, vars := range idx {
		t.Logf("index: tpl=%s vars=%d", tpl, len(vars))
		for _, v := range vars {
			t.Logf("   var %s type=%s fields=%d", v.Name, v.TypeStr, len(v.Fields))
			for _, f := range v.Fields {
				t.Logf("      field %s type=%s nested=%d", f.Name, f.TypeStr, len(f.Fields))
			}
		}
	}

	// Validate footer.html through the render-var index like the daemon's validateTemplate path
	absFooter := filepath.Join(dir, "templates", "views", "partials", "footer.html")
	rel := "views/partials/footer.html"
	for key, vars := range idx {
		if key == rel || filepath.Base(key) == filepath.Base(rel) {
			content, _ := os.ReadFile(absFooter)
			errs := validator.ValidateTemplateFileStr(string(content), vars, rel, dir, "templates", namedBlocks, funcMapReg)
			for _, e := range errs {
				t.Logf("VALIDATE ERR: %s | var=%s", e.Message, e.Variable)
			}
			if len(errs) != 0 {
				t.Errorf("expected 0 errors validating footer.html, got %d", len(errs))
			}
		}
	}
}
