package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"testing"
	"time"
)

func memStats(label string) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fmt.Printf("[MEM] %s: heapInUse=%dMB heapAlloc=%dMB totalAlloc=%dMB numGC=%d\n",
		label, ms.HeapInuse>>20, ms.HeapAlloc>>20, ms.TotalAlloc>>20, ms.NumGC)
}

func TestDaemonMemory(t *testing.T) {
	runtime.GC()
	memStats("start")

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	done := make(chan error, 1)
	go func() {
		done <- runDaemon(stdinR, stdoutW)
	}()

	reader := bufio.NewReader(stdoutR)
	writer := bufio.NewWriter(stdinW)

	send := func(id int64, method string, params map[string]any) map[string]any {
		req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
		b, _ := json.Marshal(req)
		writer.Write(append(b, '\n'))
		writer.Flush()
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var resp struct {
			ID     int64          `json:"id"`
			Result map[string]any `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(line, &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if resp.Error != nil {
			t.Fatalf("rpc error: %v", resp.Error.Message)
		}
		return resp.Result
	}

	base := map[string]any{
		"dir":                 "/home/nabiizy/Code/go/eclinichmsgo",
		"templateRoot":        "templates",
		"contextFile":         "/home/nabiizy/Code/go/eclinichmsgo/gotpl.json",
		"validate":            true,
		"renderFunctionNames": []string{"Render", "renderPage", "ExecuteTemplate", "Execute"},
		"setFunctionNames":    []string{"Set", "Locals"},
		"contextTypeNames":    []string{"Context", "Ctx"},
	}

	for i := 1; i <= 3; i++ {
		start := time.Now()
		res := send(int64(i), "analyze", base)
		rcs, _ := res["renderCalls"].([]any)
		fmt.Printf("analyze#%d done in %v (renderCalls=%d)\n", i, time.Since(start), len(rcs))
		runtime.GC()
		memStats(fmt.Sprintf("after analyze#%d", i))
	}

	send(4, "shutdown", nil)
	stdinW.Close()
	<-done
}
