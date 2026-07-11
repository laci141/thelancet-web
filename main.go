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

// handleAffiliations mirrors GET /affiliations?journal=&years=&threshold=&limit=.
func handleAffiliations(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
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
	runCLI(w, r, args)
}

// handleAuthors mirrors GET /authors?institution=&journal=&limit=.
func handleAuthors(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	args := []string{"rank-authors", "--json", "--db", dbPath()}
	if v := strings.TrimSpace(q.Get("institution")); v != "" {
		args = append(args, "--institution", v)
	}
	if v := strings.TrimSpace(q.Get("journal")); v != "" {
		args = append(args, "--journal", v)
	}
	if a, err := intFlag(q.Get("limit"), "limit", 1, 500); err != nil {
		writeErr(w, err)
		return
	} else {
		args = append(args, a...)
	}
	runCLI(w, r, args)
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

func runCLI(w http.ResponseWriter, r *http.Request, args []string) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
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
		writeErr(w, fmt.Errorf("analytics failed: %s", msg))
		return
	}
	raw := bytes.TrimSpace(stdout.Bytes())
	if !json.Valid(raw) {
		writeErr(w, errors.New("CLI returned non-JSON output"))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(raw)
}

func writeErr(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusBadGateway)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
