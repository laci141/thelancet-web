package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// defaultCLISlots is how many child CLI processes may run at once.
//
// Four, measured rather than chosen for roundness. The host is a two-core CX23
// and a single CLI run peaks near 12% of one core, so saturation arithmetic
// points to roughly sixteen concurrent runs. Sixteen is the SATURATION point,
// not the SAFE one: seven other apps share the same two cores, and a saturated
// host makes Docker's healthcheck queue behind the work it is meant to check.
// Three failed checks mark a container unhealthy — today only a label, but one
// that monitoring will eventually act on.
//
// The cost of leaving this unbounded was measured on corpova: a hundred
// concurrent distinct requests made the median caller pay +1.14s purely to
// contention, on a machine with MORE cores than the CX23 and against a stub
// doing no real work. That figure is a floor.
const defaultCLISlots = 4

// cliSlotWait is how long a request waits for a free slot before giving up.
//
// Unlike the slot count, this is NOT measured — it is a judgement, and saying
// so is the point. A brief queue absorbs bursts; an unbounded one would let a
// request sit until its own deadline expired and then fail anyway, having held
// a connection throughout. A var so tests can shorten it.
var cliSlotWait = 3 * time.Second

// cliSlotRetryAfter is the Retry-After value sent with a 503, in seconds.
const cliSlotRetryAfter = 30

// errCLIBusy marks a run rejected for lack of a slot rather than a CLI failure.
// The two must not share a status code: 502 says the CLI broke, 503 says the
// server is full and the same request will work shortly.
var errCLIBusy = errors.New("server is busy running other analyses; please retry shortly")

// cliSem bounds child-process concurrency for the whole server.
var cliSem = newCLISemaphore(cliSlotsFromEnv())

// cliSlotsFromEnv reads CLI_MAX_CONCURRENT. Zero or negative disables the bound
// entirely, restoring pre-semaphore behaviour — the escape hatch if this ever
// turns out wrong in production, reachable with an env var and a restart rather
// than a deploy.
func cliSlotsFromEnv() int {
	raw := strings.TrimSpace(os.Getenv("CLI_MAX_CONCURRENT"))
	if raw == "" {
		return defaultCLISlots
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return defaultCLISlots
	}
	return n
}

// cliSemaphore is a counting semaphore over child CLI processes.
//
// A buffered channel rather than mutex bookkeeping, because acquisition must be
// selectable against both a timeout and a context, and a channel send is the
// only primitive that composes with select.
//
// FUTURE, AND DELIBERATELY NOT BUILT YET. Once requests carry an identity,
// admin and paid tiers should not queue behind free ones. The intended shape is
// a RESERVED SLOT: of the four, one reachable only by admin and Max. That keeps
// the call site in runCLI unchanged — acquire simply learns a tier argument.
//
// What should NOT be built is a strict priority queue: with paid traffic
// arriving steadily a free request never reaches the head of the queue and
// eventually takes the 503 it was waiting to avoid. A reserved slot bounds the
// harm instead.
type cliSemaphore struct {
	ch chan struct{}
}

// newCLISemaphore returns a semaphore of n slots. n <= 0 yields a disabled one
// whose acquire always succeeds immediately.
func newCLISemaphore(n int) *cliSemaphore {
	if n <= 0 {
		return &cliSemaphore{}
	}
	return &cliSemaphore{ch: make(chan struct{}, n)}
}

// acquire takes a slot, waiting up to cliSlotWait for one. Exhaustion returns
// errCLIBusy; a cancelled context returns its own error, so a client that went
// away is not reported as an overload.
func (s *cliSemaphore) acquire(ctx context.Context) error {
	if s == nil || s.ch == nil {
		return nil
	}

	// Fast path: a free slot costs no timer and no second select.
	select {
	case s.ch <- struct{}{}:
		return nil
	default:
	}

	t := time.NewTimer(cliSlotWait)
	defer t.Stop()

	select {
	case s.ch <- struct{}{}:
		return nil
	case <-t.C:
		return errCLIBusy
	case <-ctx.Done():
		return ctx.Err()
	}
}

// release returns a slot. Safe on a disabled semaphore, so callers may defer it
// unconditionally.
func (s *cliSemaphore) release() {
	if s == nil || s.ch == nil {
		return
	}
	select {
	case <-s.ch:
	default:
	}
}

// inUse reports how many slots are held. Tests and logging only; the value is a
// snapshot and stale the moment it is read.
func (s *cliSemaphore) inUse() int {
	if s == nil || s.ch == nil {
		return 0
	}
	return len(s.ch)
}

// capacity reports the configured slot count, 0 when the bound is disabled.
func (s *cliSemaphore) capacity() int {
	if s == nil || s.ch == nil {
		return 0
	}
	return cap(s.ch)
}

// writeCLIError turns a runCLI failure into an HTTP response, separating "the
// server is full" from "the CLI broke".
//
// A 503 without a Retry-After is not actionable: a client that retries
// immediately makes the overload it just hit worse.
func writeCLIError(w http.ResponseWriter, err error) {
	if errors.Is(err, errCLIBusy) {
		w.Header().Set("Retry-After", strconv.Itoa(cliSlotRetryAfter))
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	http.Error(w, err.Error(), http.StatusBadGateway)
}
