package rawfetch

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rxbynerd/dressage/internal/conversation"
	"github.com/rxbynerd/dressage/internal/summary"
)

// writeFile writes data to dir/name and stamps it with the given modification
// time so tests can control the request/response ordering that pairing relies on.
func writeFile(t *testing.T, dir, name string, data []byte, mtime time.Time) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", name, err)
	}
	return path
}

// requestBody builds a Messages API request body with numMsgs messages, a
// JSON-object user_id carrying the session, and an optional previous_message_id.
func requestBody(t *testing.T, modelID, sessionID, prevID string, numMsgs int) []byte {
	t.Helper()
	msgs := make([]map[string]any, 0, numMsgs)
	for i := range numMsgs {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, map[string]any{"role": role, "content": "m"})
	}
	userID, _ := json.Marshal(map[string]string{
		"device_id":    "dev-1",
		"account_uuid": "acct-1",
		"session_id":   sessionID,
	})
	req := map[string]any{
		"model":       modelID,
		"messages":    msgs,
		"metadata":    map[string]string{"user_id": string(userID)},
		"diagnostics": map[string]any{"previous_message_id": prevID},
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return data
}

// responseBody builds a non-streaming Messages API response body.
func responseBody(t *testing.T, modelID, msgID string, inTok, outTok, cacheRead int64) []byte {
	t.Helper()
	resp := map[string]any{
		"id":          msgID,
		"type":        "message",
		"role":        "assistant",
		"model":       modelID,
		"stop_reason": "end_turn",
		"content":     []map[string]any{{"type": "text", "text": "reply " + msgID}},
		"usage": map[string]any{
			"input_tokens":            inTok,
			"output_tokens":           outTok,
			"cache_read_input_tokens": cacheRead,
		},
	}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	return data
}

// buildThreeTurnSession lays down a three-turn session: two turns pair by the
// message-id chain and the terminal turn pairs by write time. Returns the dir.
func buildThreeTurnSession(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	base := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	const model = "claude-test"

	// Turn 1: 1 message, first turn (no prev). Response msg_1.
	writeFile(t, dir, "aaa.request.json", requestBody(t, model, "sess-1", "", 1), base)
	writeFile(t, dir, "req_A.response.json", responseBody(t, model, "msg_1", 10, 100, 5), base.Add(time.Second))

	// Turn 2: 3 messages, prev=msg_1. Response msg_2.
	writeFile(t, dir, "bbb.request.json", requestBody(t, model, "sess-1", "msg_1", 3), base.Add(10*time.Second))
	writeFile(t, dir, "req_B.response.json", responseBody(t, model, "msg_2", 20, 200, 6), base.Add(11*time.Second))

	// Turn 3 (terminal): 5 messages, prev=msg_2. Response msg_3 pairs by mtime.
	writeFile(t, dir, "ccc.request.json", requestBody(t, model, "sess-1", "msg_2", 5), base.Add(20*time.Second))
	writeFile(t, dir, "req_C.response.json", responseBody(t, model, "msg_3", 30, 300, 7), base.Add(21*time.Second))
	return dir
}

func TestFetchPairsChainAndTerminal(t *testing.T) {
	dir := buildThreeTurnSession(t)

	records, err := New(dir).Fetch(context.Background(), time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("got %d records, want 3", len(records))
	}

	// Records are sorted by timestamp: turn 1, 2, 3.
	want := []struct {
		apiReqID string
		inTok    int64
		outTok   int64
		cacheR   int64
		msgID    string
	}{
		{"req_A", 10, 100, 5, "msg_1"},
		{"req_B", 20, 200, 6, "msg_2"},
		{"req_C", 30, 300, 7, "msg_3"},
	}
	for i, w := range want {
		rec := records[i]
		if rec.Provider != provider {
			t.Errorf("record %d provider = %q, want %q", i, rec.Provider, provider)
		}
		if rec.RequestID != w.apiReqID {
			t.Errorf("record %d RequestID = %q, want %q (pairing failed)", i, rec.RequestID, w.apiReqID)
		}
		if rec.Input.TokenCount != w.inTok || rec.Output.TokenCount != w.outTok {
			t.Errorf("record %d tokens = (%d,%d), want (%d,%d)", i, rec.Input.TokenCount, rec.Output.TokenCount, w.inTok, w.outTok)
		}
		if rec.Input.CacheRead != w.cacheR {
			t.Errorf("record %d cacheRead = %d, want %d", i, rec.Input.CacheRead, w.cacheR)
		}
		if len(rec.Output.JSON) == 0 {
			t.Errorf("record %d has no paired response body", i)
			continue
		}
		var got struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(rec.Output.JSON, &got); err != nil {
			t.Errorf("record %d output not JSON: %v", i, err)
		} else if got.ID != w.msgID {
			t.Errorf("record %d output id = %q, want %q", i, got.ID, w.msgID)
		}
		if sid := conversation.ExtractSessionID(rec.Provider, rec.ModelID, rec.Input.JSON); sid != "sess-1" {
			t.Errorf("record %d session = %q, want sess-1", i, sid)
		}
		if rec.Identity.Principal != "acct-1" {
			t.Errorf("record %d principal = %q, want acct-1", i, rec.Identity.Principal)
		}
		if rec.Identity.Extra["session_id"] != "sess-1" || rec.Identity.Extra["device_id"] != "dev-1" {
			t.Errorf("record %d identity extra = %v", i, rec.Identity.Extra)
		}
	}
}

func TestFetchReconstructsConversation(t *testing.T) {
	dir := buildThreeTurnSession(t)
	records, err := New(dir).Fetch(context.Background(), time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	rpt := summary.Summarize(records)
	var convs int
	for _, d := range rpt.Days {
		convs += len(d.Conversations)
		for _, c := range d.Conversations {
			if c.SessionID != "sess-1" {
				t.Errorf("conversation session = %q, want sess-1", c.SessionID)
			}
			if c.Detail == nil {
				t.Fatalf("conversation %s was not reconstructed", c.ID)
			}
			// Terminal request has 5 history messages; the final assistant reply
			// (from the mtime-paired response) is appended as a 6th turn.
			if len(c.Detail.Turns) != 6 {
				t.Errorf("got %d turns, want 6", len(c.Detail.Turns))
			}
			last := c.Detail.Turns[len(c.Detail.Turns)-1]
			if last.Role != "assistant" || !last.HasText() {
				t.Errorf("final turn = %+v, want assistant text turn", last)
			}
			if last.Metrics == nil || last.Metrics.StopReason != "end_turn" {
				t.Errorf("final turn metrics = %+v, want stop_reason end_turn", last.Metrics)
			}
		}
	}
	if convs != 1 {
		t.Fatalf("got %d conversations, want 1", convs)
	}
	if rpt.TotalStats.InputTokens != 60 || rpt.TotalStats.OutputTokens != 600 {
		t.Errorf("token totals = (%d,%d), want (60,600)", rpt.TotalStats.InputTokens, rpt.TotalStats.OutputTokens)
	}
}

func TestFetchDateWindowFiltersRequests(t *testing.T) {
	dir := buildThreeTurnSession(t)
	// Window covering only turn 1 (base 12:00:00) — turns 2 and 3 are seconds
	// later but still the same day, so use a sub-day instant window instead.
	start := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 4, 12, 0, 5, 0, time.UTC) // [12:00:00, 12:00:05)

	records, err := New(dir).Fetch(context.Background(), start, end)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d records in window, want 1 (only turn 1)", len(records))
	}
	if records[0].RequestID != "req_A" {
		t.Errorf("windowed record RequestID = %q, want req_A", records[0].RequestID)
	}
}

func TestFetchTerminalUnpairedOutsideWindow(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	const model = "claude-test"
	// A single-turn session whose response is written far later than the
	// terminalMatchWindow: it must remain unpaired.
	writeFile(t, dir, "solo.request.json", requestBody(t, model, "sess-x", "", 1), base)
	writeFile(t, dir, "req_Z.response.json", responseBody(t, model, "msg_z", 9, 9, 0), base.Add(2*terminalMatchWindow))

	records, err := New(dir).Fetch(context.Background(), time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	rec := records[0]
	if len(rec.Output.JSON) != 0 || rec.Output.TokenCount != 0 {
		t.Errorf("expected unpaired terminal, got output len=%d tokens=%d", len(rec.Output.JSON), rec.Output.TokenCount)
	}
	// Falls back to the request UUID when there is no paired response id.
	if rec.RequestID != "solo" {
		t.Errorf("RequestID = %q, want request uuid 'solo'", rec.RequestID)
	}
}

func TestFetchSkipsMalformedAndForeignFiles(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	writeFile(t, dir, "good.request.json", requestBody(t, "claude-test", "sess-1", "", 1), base)
	writeFile(t, dir, "bad.request.json", []byte("{not json"), base)
	writeFile(t, dir, "empty.request.json", []byte(`{"model":"m","messages":[]}`), base)
	writeFile(t, dir, "notes.txt", []byte("ignore me"), base)

	records, err := New(dir).Fetch(context.Background(), time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1 (only the well-formed request)", len(records))
	}
}

func TestFetchMissingDirErrors(t *testing.T) {
	_, err := New(filepath.Join(t.TempDir(), "does-not-exist")).Fetch(context.Background(), time.Time{}, time.Time{})
	if err == nil {
		t.Fatal("expected error for missing capture directory")
	}
}

func TestParseUserIDForms(t *testing.T) {
	blob := parseUserID(`{"device_id":"d","account_uuid":"a","session_id":"s"}`)
	if blob.AccountUUID != "a" || blob.DeviceID != "d" || blob.SessionID != "s" {
		t.Errorf("parseUserID JSON form = %+v", blob)
	}
	// Legacy flat form carries no account/device attributes.
	if got := parseUserID("user_hash_account__session_abc"); got != (userIDBlob{}) {
		t.Errorf("parseUserID legacy form = %+v, want zero", got)
	}
}

// requestBodyFirstMsg is requestBody with a controllable first message, so
// tests can lay down distinct chains (main thread vs subagent sidechains)
// within one session.
func requestBodyFirstMsg(t *testing.T, modelID, sessionID, prevID, firstMsg string, numMsgs int) []byte {
	t.Helper()
	msgs := make([]map[string]any, 0, numMsgs)
	msgs = append(msgs, map[string]any{"role": "user", "content": firstMsg})
	for i := 1; i < numMsgs; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, map[string]any{"role": role, "content": "m"})
	}
	userID, _ := json.Marshal(map[string]string{
		"device_id":    "dev-1",
		"account_uuid": "acct-1",
		"session_id":   sessionID,
	})
	req := map[string]any{
		"model":       modelID,
		"messages":    msgs,
		"metadata":    map[string]string{"user_id": string(userID)},
		"diagnostics": map[string]any{"previous_message_id": prevID},
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return data
}

// TestFetchParallelSidechains pins the chain-aware pairing: a subagent chain
// interleaved with the main thread must not steal the main thread's response
// (the pre-chain adjacency pairing paired by message-count order across the
// whole session and misattributed exactly this shape).
func TestFetchParallelSidechains(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	const model = "claude-test"

	// Main thread turn 1: 1 message at t+0, response msg_m1.
	writeFile(t, dir, "main1.request.json", requestBodyFirstMsg(t, model, "sess-p", "", "main task", 1), base)
	writeFile(t, dir, "req_M1.response.json", responseBody(t, model, "msg_m1", 10, 100, 0), base.Add(time.Second))

	// Subagent chain: 1 message at t+5s (interleaves before main turn 2),
	// response msg_s1 with distinct token counts.
	writeFile(t, dir, "sub1.request.json", requestBodyFirstMsg(t, model, "sess-p", "", "subagent task", 1), base.Add(5*time.Second))
	writeFile(t, dir, "req_S1.response.json", responseBody(t, model, "msg_s1", 99, 999, 0), base.Add(6*time.Second))

	// Main thread turn 2: 3 messages at t+10s, prev names main turn 1's
	// response. Its own response msg_m2 arrives at t+11s.
	writeFile(t, dir, "main2.request.json", requestBodyFirstMsg(t, model, "sess-p", "msg_m1", "main task", 3), base.Add(10*time.Second))
	writeFile(t, dir, "req_M2.response.json", responseBody(t, model, "msg_m2", 20, 200, 0), base.Add(11*time.Second))

	records, err := New(dir).Fetch(context.Background(), time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("got %d records, want 3", len(records))
	}

	// Sorted by mtime: main1, sub1, main2.
	byReq := map[string]int{"req_M1": 0, "req_S1": 1, "req_M2": 2}
	for want, i := range byReq {
		if records[i].RequestID != want {
			t.Errorf("record %d RequestID = %q, want %q (chain pairing failed)", i, records[i].RequestID, want)
		}
	}
	if got := records[1].Input.TokenCount; got != 99 {
		t.Errorf("subagent input tokens = %d, want 99 (response misattributed)", got)
	}
	if got := records[0].Input.TokenCount; got != 10 {
		t.Errorf("main turn 1 input tokens = %d, want 10", got)
	}

	// Thread ids: both main turns share the main root's uuid; the subagent has
	// its own.
	if records[0].Correlation.ThreadID != "main1" || records[2].Correlation.ThreadID != "main1" {
		t.Errorf("main thread ids = (%q, %q), want (main1, main1)",
			records[0].Correlation.ThreadID, records[2].Correlation.ThreadID)
	}
	if records[1].Correlation.ThreadID != "sub1" {
		t.Errorf("subagent thread id = %q, want sub1", records[1].Correlation.ThreadID)
	}

	// Correlation lift: message ids and stop reason come from the response
	// envelope; the transcript length from the request.
	if records[0].Correlation.MessageID != "msg_m1" || records[0].StopReason != "end_turn" {
		t.Errorf("record 0 correlation = %+v stop=%q", records[0].Correlation, records[0].StopReason)
	}
	if records[2].Correlation.PrevMessageID != "msg_m1" || records[2].Correlation.NumMessages != 3 {
		t.Errorf("record 2 correlation = %+v", records[2].Correlation)
	}
}

// TestFetchCompactionStitchesThread pins the compaction link: a rewritten
// transcript arrives as a new chain whose root names the old context's final
// response; the old tip must claim that response (even far outside the
// write-time window) and both chains must share one thread id.
func TestFetchCompactionStitchesThread(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	const model = "claude-test"

	// Old context: single turn, response written 1s later.
	writeFile(t, dir, "old.request.json", requestBodyFirstMsg(t, model, "sess-c", "", "original prompt", 1), base)
	writeFile(t, dir, "req_OLD.response.json", responseBody(t, model, "msg_old", 10, 100, 0), base.Add(time.Second))

	// Continuation after compaction: different first message (rewritten
	// transcript), root prev names msg_old, written well outside the
	// write-time fallback window so only the id link can pair the old tip.
	late := base.Add(3 * terminalMatchWindow)
	writeFile(t, dir, "new.request.json", requestBodyFirstMsg(t, model, "sess-c", "msg_old", "compacted summary", 1), late)
	writeFile(t, dir, "req_NEW.response.json", responseBody(t, model, "msg_new", 20, 200, 0), late.Add(time.Second))

	records, err := New(dir).Fetch(context.Background(), time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2", len(records))
	}
	if records[0].RequestID != "req_OLD" {
		t.Errorf("old tip RequestID = %q, want req_OLD (compaction link did not pair it)", records[0].RequestID)
	}
	if records[0].Correlation.ThreadID != "old" || records[1].Correlation.ThreadID != "old" {
		t.Errorf("thread ids = (%q, %q), want both 'old' (stitching failed)",
			records[0].Correlation.ThreadID, records[1].Correlation.ThreadID)
	}
}
