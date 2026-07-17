// Command thelancet-web serves a single-page UI plus two JSON endpoints,
// GET /affiliations and GET /authors, that mirror the thelancet CLI's `serve`
// mode. Rather than proxying a subprocess `serve` (which binds loopback only and
// serves no static files), it shells out to the equivalent analytics commands
// (`affiliation-growth` / `rank-authors --json`) against a local mirror DB. This
// keeps the UI and API same-origin on one port — the shape Render deploys.
//
// The endpoints are read-only and keyless (thelancet analytics take no LLM key).
// Query params are whitelisted and passed as discrete argv elements (no shell),
// so untrusted input can never inject flags or commands.
//
// Post-processing (done here, not in the CLI):
//   - /authors:      minWorks filter (default 2) removes single-consortium-paper
//     authors (e.g. Global Burden of Disease papers with 500+ co-authors).
//   - /affiliations: minPrior grouping (default 5) moves institutions with a
//     tiny prior-window base to the bottom, flagged low_base / is_new, so a
//     1 -> 22 jump can't show up as a misleading "+2100%".
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
// minPrior does NOT drop rows: institutions whose prior-window count is below it
// keep their data but are flagged (low_base, is_new when prior==0) and moved to
// the bottom of the list, so growth % stays meaningful at the top.
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
		// Unexpected shape (e.g. error object) — pass through unchanged.
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
// minWorks (default 2) filters out authors below the works threshold BEFORE the
// final limit is applied; the CLI is over-fetched (limit*5, capped at 500) so
// the response can still fill the requested limit after filtering.
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

// runCLIRaw executes the CLI and returns its validated JSON stdout.
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
