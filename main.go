// Command thelancet-web serves a single-page UI plus six JSON endpoints:
// GET /affiliations, /authors, /drift, /curate, /mesh, and /check, that mirror
// the thelancet CLI's analytics commands. The /check endpoint integrates the
// retraction-checker CLI for batch verification. All endpoints are read-only and
// keyless (analytics take no LLM key). Query params are whitelisted and passed
// as discrete argv elements (no shell).
//
// Post-processing (done here, not in the CLI):
//   - /authors:      minWorks filter removes single-consortium-paper authors.
//   - /affiliations: minPrior grouping moves institutions with tiny prior base.
//   - /drift:        passes through topic-share deltas between year windows.
//   - /curate:       ranked reading lists for a topic, optionally scoped to a
//     journal; also feeds the Rising Papers view (citations-per-year is computed
//     client-side from the year + cited_by_count fields).
//   - /mesh:         co-authorship pairs within an institution (feeds the D3
//     force-directed collaboration graph, which is built client-side).
//   - /check:        verifies retraction status via the retraction-checker CLI.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func cliBinaryPath() string {
	if p := strings.TrimSpace(os.Getenv("CLI_BIN")); p != "" {
		return p
	}
	name := "thelancet-pp-cli"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join("bin", name)
}

func retractionCheckerBinaryPath() string {
	if p := strings.TrimSpace(os.Getenv("RETRACTION_CHECKER_BIN")); p != "" {
		return p
	}
	name := "retraction-checker-pp-cli"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	// Try local submodule path first, then system PATH.
	local := filepath.Join("bin", name)
	if _, err := os.Stat(local); err == nil {
		return local
	}
	return name // Fall back to PATH.
}

// dbPath is the mirror the analytics commands read (populated by `refresh`).
func dbPath() string {
	if p := strings.TrimSpace(os.Getenv("THELANCET_DB")); p != "" {
		return p
	}
	return "data.db"
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", handleRoot)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/affiliations", handleAffiliations)
	mux.HandleFunc("/authors", handleAuthors)
	mux.HandleFunc("/drift", handleDrift)
	mux.HandleFunc("/curate", handleCurate)
	mux.HandleFunc("/mesh", handleMesh)
	mux.HandleFunc("/check", handleCheck)

	addr := "127.0.0.1:8080"
	if a := strings.TrimSpace(os.Getenv("ADDR")); a != "" {
		addr = a
	} else if p := strings.TrimSpace(os.Getenv("PORT")); p != "" {
		addr = "0.0.0.0:" + p
	}
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("thelancet-web listening on %s (CLI: %s, DB: %s)", addr, cliBinaryPath(), dbPath())
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}

func handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(w, r)
		return
	}
	if data, err := os.ReadFile("index.html"); err == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte("ok"))
}

// handleAffiliations mirrors GET /affiliations?journal=&years=&threshold=&limit=&minPrior=.
func handleAffiliations(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	minPrior, err := optInt(q.Get("minPrior"), 5, 0, 20)
	if err != nil {
		writeErr(w, err)
		return
	}
	args := []string{"affiliation-growth", "--json", "--db", dbPath()}
	if v := strings.TrimSpace(q.Get("journal")); v != "" {
		args = append(args, "--journal", v)
	}
	if a, err := intFlag(q.Get("years"), "years", 1, 100); err != nil {
		writeErr(w, err)
		return
	} else {
		args = append(args, a...)
	}
	if a, err := intFlag(q.Get("threshold"), "threshold", 1, 1000); err != nil {
		writeErr(w, err)
		return
	} else {
		args = append(args, a...)
	}
	if a, err := intFlag(q.Get("limit"), "limit", 1, 500); err != nil {
		writeErr(w, err)
		return
	} else {
		args = append(args, a...)
	}

	raw, err := runCLIRaw(r.Context(), args)
	if err != nil {
		writeErr(w, err)
		return
	}

	var rows []map[string]any
	if json.Unmarshal(raw, &rows) != nil {
		writeRaw(w, raw)
		return
	}
	normal := make([]map[string]any, 0, len(rows))
	lowBase := make([]map[string]any, 0)
	for _, row := range rows {
		prior := jsonInt(row["prior_count"])
		if prior >= minPrior {
			normal = append(normal, row)
			continue
		}
		row["low_base"] = true
		if prior == 0 {
			row["is_new"] = true
		}
		lowBase = append(lowBase, row)
	}
	writeJSONValue(w, append(normal, lowBase...))
}

// handleAuthors mirrors GET /authors?institution=&journal=&limit=&minWorks=.
func handleAuthors(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	minWorks, err := optInt(q.Get("minWorks"), 2, 1, 10)
	if err != nil {
		writeErr(w, err)
		return
	}
	limit, err := optInt(q.Get("limit"), 25, 1, 500)
	if err != nil {
		writeErr(w, err)
		return
	}
	cliLimit := limit * 5
	if cliLimit > 500 {
		cliLimit = 500
	}

	args := []string{"rank-authors", "--json", "--db", dbPath()}
	if v := strings.TrimSpace(q.Get("institution")); v != "" {
		args = append(args, "--institution", v)
	}
	if v := strings.TrimSpace(q.Get("journal")); v != "" {
		args = append(args, "--journal", v)
	}
	args = append(args, "--limit", strconv.Itoa(cliLimit))

	raw, err := runCLIRaw(r.Context(), args)
	if err != nil {
		writeErr(w, err)
		return
	}

	var rows []map[string]any
	if json.Unmarshal(raw, &rows) != nil {
		writeRaw(w, raw)
		return
	}
	filtered := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if jsonInt(row["works"]) >= minWorks {
			filtered = append(filtered, row)
		}
	}
	if len(filtered) > limit {
		filtered = filtered[:limit]
	}
	writeJSONValue(w, filtered)
}

// handleDrift mirrors GET /drift?journal=&window1=&window2=&topN=.
func handleDrift(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	w1 := strings.TrimSpace(q.Get("window1"))
	w2 := strings.TrimSpace(q.Get("window2"))
	if !validYearWindow(w1) || !validYearWindow(w2) {
		writeErr(w, errors.New("window1 and window2 must be YYYY:YYYY (e.g. 2015:2019)"))
		return
	}

	topN, err := optInt(q.Get("topN"), 15, 1, 40)
	if err != nil {
		writeErr(w, err)
		return
	}

	args := []string{"drift", "--json", "--db", dbPath(),
		"--window1", w1, "--window2", w2, "--top-n", strconv.Itoa(topN)}
	if v := strings.TrimSpace(q.Get("journal")); v != "" {
		args = append(args, "--journal", v)
	}

	raw, err := runCLIRaw(r.Context(), args)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeRaw(w, raw)
}

// handleCurate mirrors GET /curate?topic=&journal=&sort=&limit=.
// Returns a ranked reading list (title, DOI, year, citations, etc) for a topic,
// optionally scoped to a single journal. Powers both the Reading List module
// and the Rising Papers module (which re-ranks the same rows by citations/year).
func handleCurate(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	topic := strings.TrimSpace(q.Get("topic"))
	if topic == "" {
		writeErr(w, errors.New("topic parameter required"))
		return
	}

	limit, err := optInt(q.Get("limit"), 25, 1, 100)
	if err != nil {
		writeErr(w, err)
		return
	}

	args := []string{"curate", "--topic", topic, "--json", "--db", dbPath(),
		"--limit", strconv.Itoa(limit)}
	if j := strings.TrimSpace(q.Get("journal")); j != "" {
		args = append(args, "--journal", j)
	}
	if s := strings.TrimSpace(q.Get("sort")); s != "" {
		args = append(args, "--sort", s)
	}

	raw, err := runCLIRaw(r.Context(), args)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeRaw(w, raw)
}

// handleMesh mirrors GET /mesh?org=&limit=.
// Returns co-authorship pairs within an institution ranked by shared works.
// Each row is {author_a, author_b, shared_works}; the client builds a graph
// (nodes = authors, links = shared_works) from these pairs.
func handleMesh(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	org := strings.TrimSpace(q.Get("org"))
	if org == "" {
		writeErr(w, errors.New("org parameter required"))
		return
	}

	limit, err := optInt(q.Get("limit"), 25, 1, 100)
	if err != nil {
		writeErr(w, err)
		return
	}

	args := []string{"mesh", "--json", "--db", dbPath(),
		"--org", org, "--limit", strconv.Itoa(limit)}

	raw, err := runCLIRaw(r.Context(), args)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeRaw(w, raw)
}

// handleCheck mirrors GET /check?doi= or /check?pmid=.
// Calls the retraction-checker CLI and returns retraction status.
func handleCheck(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	doi := strings.TrimSpace(q.Get("doi"))
	pmid := strings.TrimSpace(q.Get("pmid"))

	if doi == "" && pmid == "" {
		writeErr(w, errors.New("doi or pmid parameter required"))
		return
	}

	args := []string{"check", "--json"}
	if doi != "" {
		args = append(args, doi)
	} else {
		args = append(args, pmid)
	}

	// 10-second timeout for retraction check (faster than CLI analytics).
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// #nosec G204 -- fixed subcommand; doi/pmid are untrusted but passed safely.
	cmd := exec.CommandContext(ctx, retractionCheckerBinaryPath(), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// If retraction-checker is not found or fails, return a safe fallback.
		writeJSONValue(w, map[string]any{
			"retracted": false,
			"error":     "retraction-checker unavailable",
		})
		return
	}

	raw := bytes.TrimSpace(stdout.Bytes())
	if !json.Valid(raw) {
		writeJSONValue(w, map[string]any{
			"retracted": false,
			"error":     "invalid JSON from checker",
		})
		return
	}
	writeRaw(w, raw)
}

// validYearWindow accepts exactly YYYY:YYYY with 4-digit years and start <= end.
func validYearWindow(s string) bool {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return false
	}
	a, err1 := strconv.Atoi(parts[0])
	b, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return false
	}
	if a < 1900 || a > 2100 || b < 1900 || b > 2100 {
		return false
	}
	return a <= b
}

// intFlag validates an optional integer query param and returns it as a
// ["--name", "v"] pair, or an empty slice when the param was absent.
func intFlag(raw, name string, min, max int) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return nil, fmt.Errorf("%s must be an integer", name)
	}
	if n < min || n > max {
		return nil, fmt.Errorf("%s must be between %d and %d", name, min, max)
	}
	return []string{"--" + name, strconv.Itoa(n)}, nil
}

// optInt parses an optional integer query param into a value (not a flag),
// falling back to def when absent and range-checking otherwise.
func optInt(raw string, def, min, max int) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, errors.New("parameter must be an integer")
	}
	if n < min || n > max {
		return 0, fmt.Errorf("parameter must be between %d and %d", min, max)
	}
	return n, nil
}

// jsonInt reads an integer out of a decoded JSON value (numbers arrive as
// float64 from encoding/json; tolerate numeric strings too).
func jsonInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case string:
		i, _ := strconv.Atoi(strings.TrimSpace(n))
		return i
	}
	return 0
}

// runCLIRaw executes the thelancet CLI and returns its validated JSON stdout.
func runCLIRaw(parent context.Context, args []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	defer cancel()
	// #nosec G204 -- fixed subcommand + whitelisted flags with values passed as
	// discrete argv elements (no shell); ints are range-checked above.
	cmd := exec.CommandContext(ctx, cliBinaryPath(), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("analytics failed: %s", msg)
	}
	raw := bytes.TrimSpace(stdout.Bytes())
	if !json.Valid(raw) {
		return nil, errors.New("CLI returned non-JSON output")
	}
	return raw, nil
}

func writeRaw(w http.ResponseWriter, raw []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(raw)
}

func writeJSONValue(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusBadGateway)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
