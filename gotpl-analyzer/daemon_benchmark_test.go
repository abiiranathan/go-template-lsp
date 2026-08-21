package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// generateBenchmarkWorkspace creates a synthetic project with deep struct trees,
// methods, render calls, and partial templates in a temp directory.
func generateBenchmarkWorkspace(t testing.TB, numTemplates int) string {
	t.Helper()
	dir := t.TempDir()

	goMod := `module example.com/benchproject
go 1.22
`
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatal(err)
	}

	// Models with cyclic references and methods
	modelsGo := `package main

import "time"

type Role struct {
	Name        string
	Permissions []string
	CreatedAt   time.Time
}

func (r *Role) GetPermissions() []string { return r.Permissions }
func (r *Role) String() string           { return r.Name }

type Profile struct {
	Bio       string
	AvatarURL string
	Role      Role
	UpdatedAt time.Time
}

func (p *Profile) GetRole() Role { return p.Role }

type User struct {
	ID        int
	Username  string
	Email     string
	Profile   Profile
	Friends   []*User
	Metadata  map[string]any
	CreatedAt time.Time
}

func (u *User) GetProfile() Profile { return u.Profile }
func (u *User) GetFriends() []*User  { return u.Friends }
func (u *User) HasRole(role string) bool { return u.Profile.Role.Name == role }

type OrderItem struct {
	ID       string
	Name     string
	Price    float64
	Quantity int
}

func (oi *OrderItem) Total() float64 { return oi.Price * float64(oi.Quantity) }

type Order struct {
	ID        string
	Customer  User
	Items     []OrderItem
	CreatedAt time.Time
}

func (o *Order) GetCustomer() User      { return o.Customer }
func (o *Order) GetItems() []OrderItem { return o.Items }
`
	if err := os.WriteFile(filepath.Join(dir, "models.go"), []byte(modelsGo), 0644); err != nil {
		t.Fatal(err)
	}

	// Main handlers rendering templates
	var mainGo = `package main

import "net/http"

func Render(w http.ResponseWriter, name string, data any) {}

func handlers() {
	var user User
	var order Order
	var items []OrderItem
`
	for i := 0; i < numTemplates; i++ {
		mainGo += fmt.Sprintf(`	Render(nil, "view_%d.html", map[string]any{
		"User": user,
		"Order": order,
		"Items": items,
	})
`, i)
	}
	mainGo += "}\n"

	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(mainGo), 0644); err != nil {
		t.Fatal(err)
	}

	// Create templates directory
	tmplDir := filepath.Join(dir, "templates")
	if err := os.MkdirAll(tmplDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Partial template
	partialContent := `{{ define "user_card" }}
<div>
	<h3>{{ .User.Username }}</h3>
	<p>{{ .User.Profile.Bio }}</p>
	<p>Role: {{ .User.Profile.Role.Name }}</p>
</div>
{{ end }}
`
	if err := os.WriteFile(filepath.Join(tmplDir, "user_card.html"), []byte(partialContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Template files
	for i := range numTemplates {
		content := `<!DOCTYPE html>
<html>
<body>
	<h1>User: {{ .User.Username }}</h1>
	<p>Email: {{ .User.Email }}</p>
	<p>Joined: {{ .User.CreatedAt.Format "2006-01-02" }}</p>
	{{ template "user_card" . }}
	<h2>Order {{ .Order.ID }}</h2>
	{{ range .Items }}
		<div>{{ .Name }} - ${{ .Price }} x {{ .Quantity }} = ${{ .Total }}</div>
	{{ end }}
</body>
</html>`
		if err := os.WriteFile(filepath.Join(tmplDir, fmt.Sprintf("view_%d.html", i)), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

// TestDaemonMemoryLeak simulates continuous editing sessions in VS Code over stdin/stdout JSON-RPC.
func TestDaemonMemoryLeak(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping memory leak test in short mode")
	}

	workspace := generateBenchmarkWorkspace(t, 50)

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	done := make(chan error, 1)
	go func() {
		done <- runDaemon(stdinR, stdoutW)
	}()

	reader := bufio.NewReader(stdoutR)
	writer := bufio.NewWriter(stdinW)

	send := func(id int64, method string, params any) map[string]any {
		req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
		b, _ := json.Marshal(req)
		writer.Write(append(b, '\n'))
		writer.Flush()

		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatalf("failed reading daemon response: %v", err)
		}
		var resp struct {
			ID     int64          `json:"id"`
			Result map[string]any `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(line, &resp); err != nil {
			t.Fatalf("unmarshal daemon response: %v", err)
		}
		if resp.Error != nil {
			t.Fatalf("daemon returned RPC error: %v", resp.Error.Message)
		}
		return resp.Result
	}

	analyzeParams := daemonAnalyzeParams{
		Dir:          workspace,
		TemplateRoot: "templates",
		Validate:     true,
	}

	var startMem, midMem, endMem runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&startMem)

	const iterations = 2
	t.Logf("Running %d consecutive analyze & validation cycles...", iterations)

	activeFile := filepath.Join(workspace, "templates", "view_0.html")
	activeContent, _ := os.ReadFile(activeFile)

	for i := 1; i <= iterations; i++ {
		// 1. Trigger project analysis (simulating file saves)
		send(int64(i*3-2), "analyze", analyzeParams)

		// 2. Trigger live buffer validation (simulating keystrokes)
		send(int64(i*3-1), "validateTemplate", daemonValidateTemplateParams{
			AbsolutePath: activeFile,
			Content:      string(activeContent) + fmt.Sprintf("\n<!-- edit %d -->", i),
		})

		// 3. Trigger hover requests
		send(int64(i*3), "getHoverInfo", daemonGetHoverInfoParams{
			AbsolutePath: activeFile,
			Line:         4,
			Col:          15,
			Content:      string(activeContent),
		})

		if i == 5 {
			runtime.GC()
			runtime.ReadMemStats(&midMem)
		}
	}

	send(9999, "shutdown", nil)
	stdinW.Close()
	<-done

	runtime.GC()
	runtime.ReadMemStats(&endMem)

	startHeapMB := startMem.HeapAlloc / (1024 * 1024)
	midHeapMB := midMem.HeapAlloc / (1024 * 1024)
	endHeapMB := endMem.HeapAlloc / (1024 * 1024)

	t.Logf("--------------------------------------------------")
	t.Logf("Initial Heap:    %d MB", startHeapMB)
	t.Logf("Iteration 5 Heap: %d MB", midHeapMB)
	t.Logf("Final Heap:      %d MB (after %d iterations)", endHeapMB, iterations)
	t.Logf("Total Allocated: %d MB", endMem.TotalAlloc/(1024*1024))
	t.Logf("--------------------------------------------------")

	// Check for runaway heap growth (more than 300MB retained for 50 templates is excessive)
	if endHeapMB > 300 {
		t.Errorf("FAIL: Retained heap size (%d MB) indicates a severe memory leak across analyze cycles", endHeapMB)
	}
}

// BenchmarkDaemonAnalyze profiles allocation throughput during analyze cycles.
// Run with: go test -bench=BenchmarkDaemonAnalyze -benchmem -memprofile=mem.prof
func BenchmarkDaemonAnalyze(b *testing.B) {
	workspace := generateBenchmarkWorkspace(b, 30)

	d := &analyzerDaemon{
		templateOverlays: make(map[string]string),
	}

	params := daemonAnalyzeParams{
		Dir:          workspace,
		TemplateRoot: "templates",
		Validate:     true,
	}

	b.ReportAllocs()

	for b.Loop() {
		_, err := d.analyze(params)
		if err != nil {
			b.Fatalf("analyze error: %v", err)
		}
	}
}
