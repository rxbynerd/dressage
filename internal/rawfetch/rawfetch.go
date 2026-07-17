// Package rawfetch reconstructs conversations from raw Claude (Anthropic
// Messages API) request and response bodies captured on disk, and normalizes
// them into provider-neutral model.Record values.
//
// # Capture layout
//
// The capture is a flat directory (default ~/.claude/raw-api-bodies) holding two
// disjoint sets of files, one JSON body per file:
//
//   - <uuid>.request.json     — a Messages API *request* body (model, messages[],
//     system, tools, metadata.user_id, diagnostics.previous_message_id, ...).
//     Claude Code resends the entire running transcript on every turn, so the
//     latest request of a session contains the full conversation history.
//   - req_<id>.response.json  — a Messages API *response* body (id "msg_...",
//     role, content[], stop_reason, usage). These carry NO session id and NO
//     timestamp of their own.
//
// # Correlating a request with its response
//
// Filenames do not share a key: requests are named by an opaque UUID and
// responses by the API request id. The bodies are linked instead through the
// message-id chain. Each request's diagnostics.previous_message_id holds the
// "id" of the response to the *previous* turn, so within a session sorted by
// growing message count the response produced by turn i is the body identified
// by turn i+1's previous_message_id. This resolves for ~100% of captured turns.
//
// The final turn of a session has no following request to point back at its
// response, so it cannot be located by id. Those terminal responses are matched
// by write time instead: the earliest not-yet-claimed response of the same model
// written at or after the request, within terminalMatchWindow. This heuristic
// only ever affects the single last assistant turn of a session.
//
// # Timestamps
//
// Neither body carries a wall-clock timestamp, so the request file's modification
// time is used as the invocation time (it approximates when the turn was sent).
// The [start, end) date filter is applied against this mtime.
package rawfetch

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/rxbynerd/dressage/internal/fetch"
	"github.com/rxbynerd/dressage/internal/model"
)

// provider is the provider tag stamped on every record emitted by this fetcher.
// It routes to the Anthropic Messages reconstructor via the conversation
// package's default dispatch.
const provider = "claude"

// operation is the synthetic operation name recorded for each invocation. The
// raw bodies come from the Anthropic Messages endpoint, which has a single
// create operation.
const operation = "messages"

// terminalMatchWindow bounds the write-time fallback used to locate the response
// for the last (terminal) turn of a session, which has no following request to
// identify it by id. A response written more than this long after the request is
// assumed to belong to a different (or still-open) invocation and is not matched.
const terminalMatchWindow = 10 * time.Minute

// Fetcher reads raw Claude request/response bodies from a local directory and
// normalizes paired invocations into model.Record values.
type Fetcher struct {
	dir string
	// workers caps the number of files parsed concurrently. Defaults to
	// GOMAXPROCS when zero.
	workers int
}

// Fetcher implements the provider-neutral fetch.Fetcher interface.
var _ fetch.Fetcher = (*Fetcher)(nil)

// New constructs a Fetcher that reads captured bodies from dir.
func New(dir string) *Fetcher {
	return &Fetcher{dir: dir}
}

// Fetch enumerates the capture directory, decodes request bodies whose
// modification time falls in [start, end), pairs each with its response body,
// and returns the resulting normalized records. A zero start or end means
// unbounded on that side.
func (f *Fetcher) Fetch(ctx context.Context, start, end time.Time) ([]model.Record, error) {
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		return nil, fmt.Errorf("reading capture directory %q: %w", f.dir, err)
	}

	// Classify entries by suffix and apply the request-side date window up front
	// so out-of-window transcripts are never read into memory. Response files are
	// admitted on a slightly widened window: a turn's response is written shortly
	// after its request, so a request near the end boundary may pair with a
	// response written just past it.
	reqPaths, respPaths := classify(entries, f.dir, start, end)
	if len(reqPaths) == 0 {
		log.Printf("%s: no request bodies in %s for the given window", provider, f.dir)
		return nil, nil
	}
	if start.IsZero() && end.IsZero() {
		log.Printf("%s: no date filter set; scanning all %d request bodies in %s "+
			"(use --start/--end to bound memory and time on large captures)",
			provider, len(reqPaths), f.dir)
	}

	workers := f.workers
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}

	// Index responses by message id (id -> lightweight metadata + path). Bodies
	// are re-read lazily only for responses that end up paired, keeping response
	// payloads out of the resident set.
	respIndex, err := indexResponses(ctx, respPaths, workers)
	if err != nil {
		return nil, err
	}

	// Parse request bodies (retaining their raw payloads, which reconstruction
	// needs) in parallel.
	reqs, err := parseRequests(ctx, reqPaths, workers)
	if err != nil {
		return nil, err
	}
	if len(reqs) == 0 {
		return nil, nil
	}

	records := buildRecords(reqs, respIndex)
	log.Printf("%s: paired %d/%d request bodies with a response", provider, countPaired(records), len(records))
	return records, nil
}

// classify splits directory entries into request and response file paths,
// applying the [start, end) modification-time window to requests and a widened
// window to responses. Non-JSON and unrecognized entries are ignored.
func classify(entries []os.DirEntry, dir string, start, end time.Time) (reqPaths []timedPath, respPaths []timedPath) {
	// Widen the response window so a request near a boundary can still reach the
	// response written moments later (or the predecessor response moments before).
	var respStart, respEnd time.Time
	if !start.IsZero() {
		respStart = start.Add(-terminalMatchWindow)
	}
	if !end.IsZero() {
		respEnd = end.Add(terminalMatchWindow)
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		var isResp bool
		switch {
		case strings.HasSuffix(name, ".response.json"):
			isResp = true
		case strings.HasSuffix(name, ".request.json"):
			isResp = false
		default:
			continue
		}

		info, err := e.Info()
		if err != nil {
			continue // vanished between ReadDir and Info; skip
		}
		mtime := info.ModTime()
		tp := timedPath{path: filepath.Join(dir, name), mtime: mtime}

		if isResp {
			if inWindow(mtime, respStart, respEnd) {
				respPaths = append(respPaths, tp)
			}
			continue
		}
		if inWindow(mtime, start, end) {
			reqPaths = append(reqPaths, tp)
		}
	}
	return reqPaths, respPaths
}

// inWindow reports whether t is within [start, end), treating a zero bound as
// unbounded on that side.
func inWindow(t, start, end time.Time) bool {
	if !start.IsZero() && t.Before(start) {
		return false
	}
	if !end.IsZero() && !t.Before(end) {
		return false
	}
	return true
}

// timedPath is a capture file path paired with its modification time.
type timedPath struct {
	path  string
	mtime time.Time
}

// countPaired returns the number of records that carry a response body.
func countPaired(records []model.Record) int {
	n := 0
	for i := range records {
		if records[i].Output.Present() {
			n++
		}
	}
	return n
}

// forEachPath runs fn over paths using a bounded worker pool, aborting if ctx is
// cancelled. fn must be safe for concurrent use.
func forEachPath(ctx context.Context, paths []timedPath, workers int, fn func(timedPath) error) error {
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan timedPath)
	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error
	fail := func(err error) {
		once.Do(func() { firstErr = err })
	}

	for i := 0; i < workers; i++ {
		wg.Go(func() {
			for tp := range jobs {
				if ctx.Err() != nil {
					return
				}
				if err := fn(tp); err != nil {
					fail(err)
					return
				}
			}
		})
	}

feed:
	for _, tp := range paths {
		select {
		case <-ctx.Done():
			fail(ctx.Err())
			break feed
		case jobs <- tp:
		}
	}
	close(jobs)
	wg.Wait()
	return firstErr
}
