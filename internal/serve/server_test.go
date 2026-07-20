package serve

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rxbynerd/dressage/internal/ir"
	"github.com/rxbynerd/dressage/internal/model"
	"github.com/rxbynerd/dressage/internal/summary"
)

// fixtureRecords builds a claude session whose two records carry distinct
// correlation thread ids, so reconstruction yields a main thread plus one
// sidechain — exercising the sidechain rendering path — alongside a system
// prompt, a tool, and token/cache accounting.
func fixtureRecords() []model.Record {
	base := time.Date(2024, 3, 2, 14, 0, 0, 0, time.UTC)
	mainIn := `{"system":"You are a coding assistant.",` +
		`"messages":[{"role":"user","content":"Refactor the parser."}],` +
		`"tools":[{"name":"Bash","description":"Run a shell command","input_schema":{"type":"object"}}]}`
	mainOut := `{"id":"msg_m","role":"assistant","stop_reason":"tool_use","content":[{"type":"text","text":"Delegating exploration to a subagent."}]}`
	subIn := `{"messages":[{"role":"user","content":"Explore the parser module."}]}`
	subOut := `{"id":"msg_s","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"parser.go defines three functions."}]}`

	return []model.Record{
		{
			Provider: "claude", Timestamp: base, RequestID: "req-main-1",
			ModelID: "claude-opus-4-6", Operation: "messages", Status: "200",
			SessionID:   "sess-serve",
			Correlation: model.Correlation{ThreadID: "chain-main", MessageID: "msg_m", RequestUUID: "chain-main", NumMessages: 1},
			Identity:    model.Identity{Principal: "acct-serve"},
			Input:       model.Body{JSON: json.RawMessage(mainIn), ContentType: "application/json", TokenCount: 30, CacheRead: 8, CacheWrite: 3},
			Output:      model.Body{JSON: json.RawMessage(mainOut), ContentType: "application/json", TokenCount: 14},
		},
		{
			Provider: "claude", Timestamp: base.Add(5 * time.Second), RequestID: "req-sub-1",
			ModelID: "claude-opus-4-6", Operation: "messages", Status: "200",
			SessionID:   "sess-serve",
			Correlation: model.Correlation{ThreadID: "chain-explore", MessageID: "msg_s", RequestUUID: "chain-explore", NumMessages: 1},
			Identity:    model.Identity{Principal: "acct-serve"},
			Input:       model.Body{JSON: json.RawMessage(subIn), ContentType: "application/json", TokenCount: 12},
			Output:      model.Body{JSON: json.RawMessage(subOut), ContentType: "application/json", TokenCount: 9},
		},
	}
}

// newServer exports the fixture to a temp IR dir (embedding raw bodies unless
// omit is set) and returns a serve handler over it.
func newServer(t *testing.T, omitBodies bool) http.Handler {
	t.Helper()
	rpt := summary.Summarize(fixtureRecords())
	dir := t.TempDir()
	if err := ir.Export(rpt, dir, ir.SourceInfo{Provider: "claude", Command: "dressage claude --out report.ir"}, ir.ExportOptions{RawBodies: !omitBodies}); err != nil {
		t.Fatalf("export IR: %v", err)
	}
	reader, err := ir.OpenDir(dir)
	if err != nil {
		t.Fatalf("open IR: %v", err)
	}
	return New(reader).Handler()
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestIndexPage(t *testing.T) {
	h := newServer(t, false)
	rec := get(t, h, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"claude",                           // provider in the header
		"2024-03-02",                       // the day card date
		"conv-20240302",                    // a display_id
		"By Model",                         // the model breakdown section
		`href="/conversations/sess-serve"`, // the conversation link
		"cache read 8 &middot; write 3",    // the input-tokens card breakdown
		"Cache-read: <span>8</span>",       // day rollup / conversation row breakdown
		"Cache-write: <span>3</span>",      // day rollup / conversation row breakdown
	} {
		if !strings.Contains(body, want) {
			t.Errorf("index page missing %q", want)
		}
	}
}

func TestConversationPage(t *testing.T) {
	h := newServer(t, false)
	rec := get(t, h, "/conversations/sess-serve")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET conversation = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"You are a coding assistant.",           // system prompt
		"Refactor the parser.",                  // a main-thread turn
		"Bash",                                  // a tool
		"chain-explore",                         // the sidechain id
		"parser.go defines three functions.",    // a sidechain turn
		"Raw Invocations",                       // the raw section
		"/conversations/sess-serve/raw/0/input", // a raw-body link (bodies embedded)
		"Cache-read: <span>8</span>",            // header stats cache breakdown
		"Cache-write: <span>3</span>",           // header stats cache breakdown
	} {
		if !strings.Contains(body, want) {
			t.Errorf("conversation page missing %q", want)
		}
	}
}

func TestUnknownConversation404(t *testing.T) {
	h := newServer(t, false)
	if rec := get(t, h, "/conversations/does-not-exist"); rec.Code != http.StatusNotFound {
		t.Errorf("GET unknown conversation = %d, want 404", rec.Code)
	}
}

func TestRawBodyEmbedded(t *testing.T) {
	h := newServer(t, false)
	rec := get(t, h, "/conversations/sess-serve/raw/0/input")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET raw input = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("raw body Content-Type = %q, want application/json", ct)
	}
	if !json.Valid(rec.Body.Bytes()) {
		t.Errorf("raw body is not valid JSON:\n%s", rec.Body.String())
	}
}

func TestRawBodyBadDirection404(t *testing.T) {
	h := newServer(t, false)
	if rec := get(t, h, "/conversations/sess-serve/raw/0/sideways"); rec.Code != http.StatusNotFound {
		t.Errorf("GET raw with bad direction = %d, want 404", rec.Code)
	}
}

func TestRawBodyOutOfRange404(t *testing.T) {
	h := newServer(t, false)
	if rec := get(t, h, "/conversations/sess-serve/raw/99/input"); rec.Code != http.StatusNotFound {
		t.Errorf("GET raw out-of-range index = %d, want 404", rec.Code)
	}
}

func TestOmittedBodiesHideRawContent(t *testing.T) {
	h := newServer(t, true)

	// The conversation page notes bodies are absent and omits raw-body links.
	page := get(t, h, "/conversations/sess-serve")
	if page.Code != http.StatusOK {
		t.Fatalf("GET conversation = %d, want 200", page.Code)
	}
	if strings.Contains(page.Body.String(), "/raw/0/input") {
		t.Error("omitted-bodies page should not link raw bodies")
	}

	// And the raw endpoint 404s because the body was never embedded.
	if rec := get(t, h, "/conversations/sess-serve/raw/0/input"); rec.Code != http.StatusNotFound {
		t.Errorf("GET raw with omitted bodies = %d, want 404", rec.Code)
	}
}
