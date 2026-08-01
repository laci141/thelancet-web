package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func buildConcurrencyStubCLI(t *testing.T, logPath string, delay time.Duration) {
	t.Helper()
	dir := t.TempDir()
	src := `package main
import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)
func mark(p, s string) {
	if p == "" {
		return
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		fmt.Fprintln(f, s)
		f.Close()
	}
}
func main() {
	p := os.Getenv("CONC_STUB_LOG")
	mark(p, "S")
	d, _ := time.ParseDuration(os.Getenv("CONC_STUB_DELAY"))
	time.Sleep(d)
	out, _ := json.Marshal([]any{})
	fmt.Println(string(out))
	mark(p, "E")
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write stub source: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module concstub\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatalf("write stub go.mod: %v", err)
	}
	bin := filepath.Join(dir, "concstub")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build concurrency stub CLI: %v\n%s", err, out)
	}
	t.Setenv("CLI_BIN", bin)
	t.Setenv("CONC_STUB_LOG", logPath)
	t.Setenv("CONC_STUB_DELAY", delay.String())
}

func peakConcurrency(t *testing.T, logPath string) int {
	t.Helper()
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read stub log: %v", err)
	}
	cur, peak := 0, 0
	for _, line := range strings.Split(string(raw), "\n") {
		switch strings.TrimSpace(line) {
		case "S":
			cur++
			if cur > peak {
				peak = cur
			}
		case "E":
			cur--
		}
	}
	return peak
}

// getMesh drives the real handleMesh handler, which goes through runCLIRaw.
func getMesh(t *testing.T, org string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/mesh?org="+org, nil)
	rec := httptest.NewRecorder()
	handleMesh(rec, req)
	return rec.Code, rec.Body.String()
}

func fireDistinctMeshRequests(t *testing.T, n int) []int {
	t.Helper()
	codes := make([]int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			code, _ := getMesh(t, fmt.Sprintf("org%d", i))
			codes[i] = code
		}(i)
	}
	close(start)
	wg.Wait()
	return codes
}

func useSemaphore(t *testing.T, slots int, wait time.Duration) {
	t.Helper()
	prevSem, prevWait := cliSem, cliSlotWait
	cliSem, cliSlotWait = newCLISemaphore(slots), wait
	t.Cleanup(func() { cliSem, cliSlotWait = prevSem, prevWait })
}

func TestCLISemaphoreBoundsChildRuns(t *testing.T) {
	const requests = 12

	unboundedLog := filepath.Join(t.TempDir(), "unbounded.log")
	buildConcurrencyStubCLI(t, unboundedLog, 400*time.Millisecond)

	useSemaphore(t, 0, 10*time.Second)
	fireDistinctMeshRequests(t, requests)
	unbounded := peakConcurrency(t, unboundedLog)

	boundedLog := filepath.Join(t.TempDir(), "bounded.log")
	t.Setenv("CONC_STUB_LOG", boundedLog)

	useSemaphore(t, 4, 10*time.Second)
	codes := fireDistinctMeshRequests(t, requests)
	bounded := peakConcurrency(t, boundedLog)

	t.Logf("MEASURED peak concurrent child processes for %d distinct requests: unbounded=%d bounded=%d",
		requests, unbounded, bounded)

	if unbounded <= 4 {
		t.Fatalf("unbounded peak was %d, expected more than 4 — harness not producing concurrency", unbounded)
	}
	if bounded > 4 {
		t.Errorf("bounded peak = %d, want at most 4", bounded)
	}
	for i, c := range codes {
		if c != http.StatusOK {
			t.Errorf("request %d got %d, want 200", i, c)
		}
	}
}

// TestCheckFallsBackWhenBusy pins the /check behaviour under slot exhaustion.
// Unlike the analytics endpoints, /check must NOT 503: it is a helper lookup in
// the UI and an error there would break the page. It degrades to the same safe
// fallback shape it already uses when the checker binary is missing.
func TestCheckFallsBackWhenBusy(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "check.log")
	buildConcurrencyStubCLI(t, logPath, 2*time.Second)
	t.Setenv("RETRACTION_CHECKER_BIN", os.Getenv("CLI_BIN"))

	useSemaphore(t, 1, 150*time.Millisecond)

	if err := cliSem.acquire(context.Background()); err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/check?doi=10.1234/x", nil)
	rec := httptest.NewRecorder()
	handleCheck(rec, req)
	cliSem.release()

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — /check must degrade, not error", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "busy") {
		t.Errorf("body = %q, want it to mention busy", strings.TrimSpace(body))
	}
	if !strings.Contains(body, `"retracted":false`) {
		t.Errorf("body = %q, want the safe fallback shape", strings.TrimSpace(body))
	}
}

func TestCLISemaphoreDisabledIsANoOp(t *testing.T) {
	t.Setenv("CLI_MAX_CONCURRENT", "0")
	if got := cliSlotsFromEnv(); got != 0 {
		t.Fatalf("cliSlotsFromEnv() = %d, want 0", got)
	}
	s := newCLISemaphore(0)
	if got := s.capacity(); got != 0 {
		t.Errorf("capacity() = %d, want 0", got)
	}
	for i := 0; i < 50; i++ {
		if err := s.acquire(context.Background()); err != nil {
			t.Fatalf("acquire %d on disabled semaphore: %v", i, err)
		}
	}
	if got := s.inUse(); got != 0 {
		t.Errorf("inUse() = %d on disabled semaphore, want 0", got)
	}
	t.Setenv("CLI_MAX_CONCURRENT", "")
	if got := cliSlotsFromEnv(); got != defaultCLISlots {
		t.Errorf("empty env gave %d, want %d", got, defaultCLISlots)
	}
}
