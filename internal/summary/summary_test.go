package summary

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rxbynerd/dressage/internal/model"
)

func makeLog(modelID, arn string, ts time.Time, inputTokens, outputTokens int64) model.Record {
	return model.Record{
		Provider:  "bedrock",
		Timestamp: ts,
		ModelID:   modelID,
		RequestID: ts.Format(time.RFC3339Nano),
		Operation: "InvokeModel",
		Status:    "200",
		Identity:  model.Identity{Principal: arn},
		Input:     model.Body{TokenCount: inputTokens},
		Output:    model.Body{TokenCount: outputTokens},
	}
}

func TestSummarizeEmpty(t *testing.T) {
	rpt := Summarize(nil)
	if rpt == nil {
		t.Fatal("expected non-nil report")
	}
	if len(rpt.Days) != 0 {
		t.Errorf("expected 0 days, got %d", len(rpt.Days))
	}
	if rpt.TotalStats.InvocationCount != 0 {
		t.Errorf("expected 0 invocations, got %d", rpt.TotalStats.InvocationCount)
	}
}

func TestSummarizeSingleLog(t *testing.T) {
	ts := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	logs := []model.Record{
		makeLog("claude-3", "arn:aws:iam::123:user/alice", ts, 100, 200),
	}

	rpt := Summarize(logs)

	if rpt.TotalStats.InvocationCount != 1 {
		t.Errorf("expected 1 invocation, got %d", rpt.TotalStats.InvocationCount)
	}
	if rpt.TotalStats.InputTokens != 100 {
		t.Errorf("expected 100 input tokens, got %d", rpt.TotalStats.InputTokens)
	}
	if rpt.TotalStats.OutputTokens != 200 {
		t.Errorf("expected 200 output tokens, got %d", rpt.TotalStats.OutputTokens)
	}
	if len(rpt.Days) != 1 {
		t.Fatalf("expected 1 day, got %d", len(rpt.Days))
	}
	if len(rpt.Days[0].Conversations) != 1 {
		t.Errorf("expected 1 conversation, got %d", len(rpt.Days[0].Conversations))
	}
}

// Cache read/write counters aggregate into the day and total stats alongside
// the plain token counts.
func TestSummarizeCacheTokenTotals(t *testing.T) {
	ts := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	arn := "arn:aws:iam::123:user/alice"
	rec1 := makeLog("claude-3", arn, ts, 100, 200)
	rec1.Input.CacheRead = 40
	rec1.Input.CacheWrite = 15
	rec2 := makeLog("claude-3", arn, ts.Add(time.Minute), 50, 60)
	rec2.Input.CacheRead = 5

	rpt := Summarize([]model.Record{rec1, rec2})

	if rpt.TotalStats.CacheReadTokens != 45 || rpt.TotalStats.CacheWriteTokens != 15 {
		t.Errorf("total cache tokens = %d/%d, want 45/15",
			rpt.TotalStats.CacheReadTokens, rpt.TotalStats.CacheWriteTokens)
	}
	if len(rpt.Days) != 1 {
		t.Fatalf("expected 1 day, got %d", len(rpt.Days))
	}
	if day := rpt.Days[0].Stats; day.CacheReadTokens != 45 || day.CacheWriteTokens != 15 {
		t.Errorf("day cache tokens = %d/%d, want 45/15", day.CacheReadTokens, day.CacheWriteTokens)
	}
}

// Two invocations within the gap threshold should form a single conversation.
func TestConversationGroupingWithinGap(t *testing.T) {
	base := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	arn := "arn:aws:iam::123:user/alice"
	mdl := "claude-3"

	logs := []model.Record{
		makeLog(mdl, arn, base, 100, 50),
		makeLog(mdl, arn, base.Add(2*time.Minute), 80, 40), // within 5min gap
	}

	rpt := Summarize(logs)

	if len(rpt.Days) != 1 {
		t.Fatalf("expected 1 day, got %d", len(rpt.Days))
	}
	if len(rpt.Days[0].Conversations) != 1 {
		t.Errorf("expected 1 conversation (within gap), got %d", len(rpt.Days[0].Conversations))
	}
	conv := rpt.Days[0].Conversations[0]
	if conv.MessageCount != 2 {
		t.Errorf("expected 2 messages, got %d", conv.MessageCount)
	}
}

// Two invocations exceeding the gap threshold should form separate conversations.
func TestConversationGroupingAcrossGap(t *testing.T) {
	base := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	arn := "arn:aws:iam::123:user/alice"
	mdl := "claude-3"

	logs := []model.Record{
		makeLog(mdl, arn, base, 100, 50),
		makeLog(mdl, arn, base.Add(10*time.Minute), 80, 40), // exceeds 5min gap
	}

	rpt := Summarize(logs)

	if len(rpt.Days) != 1 {
		t.Fatalf("expected 1 day, got %d", len(rpt.Days))
	}
	if len(rpt.Days[0].Conversations) != 2 {
		t.Errorf("expected 2 conversations (gap exceeded), got %d", len(rpt.Days[0].Conversations))
	}
}

// Logs on different UTC days should be placed in separate day buckets.
func TestMultiDayGrouping(t *testing.T) {
	arn := "arn:aws:iam::123:user/alice"
	mdl := "claude-3"

	day1 := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	day2 := time.Date(2024, 1, 16, 10, 0, 0, 0, time.UTC)

	logs := []model.Record{
		makeLog(mdl, arn, day1, 100, 50),
		makeLog(mdl, arn, day2, 80, 40),
	}

	rpt := Summarize(logs)

	if len(rpt.Days) != 2 {
		t.Errorf("expected 2 days, got %d", len(rpt.Days))
	}
	if rpt.TotalStats.InvocationCount != 2 {
		t.Errorf("expected 2 total invocations, got %d", rpt.TotalStats.InvocationCount)
	}
}

// Different (model, ARN) pairs on the same day should always form separate conversations.
func TestDifferentIdentitiesSeparated(t *testing.T) {
	base := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	mdl := "claude-3"

	logs := []model.Record{
		makeLog(mdl, "arn:aws:iam::123:user/alice", base, 100, 50),
		makeLog(mdl, "arn:aws:iam::123:user/bob", base.Add(time.Minute), 80, 40),
	}

	rpt := Summarize(logs)

	if len(rpt.Days[0].Conversations) != 2 {
		t.Errorf("expected 2 conversations for different ARNs, got %d", len(rpt.Days[0].Conversations))
	}
}

// Errors should be counted correctly in stats.
func TestErrorCounting(t *testing.T) {
	base := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	arn := "arn:aws:iam::123:user/alice"
	mdl := "claude-3"

	errLog := makeLog(mdl, arn, base, 50, 0)
	errLog.ErrorCode = "ThrottlingException"
	errLog.Status = "429"

	logs := []model.Record{
		makeLog(mdl, arn, base.Add(-time.Minute), 100, 50),
		errLog,
	}

	rpt := Summarize(logs)

	if rpt.TotalStats.ErrorCount != 1 {
		t.Errorf("expected 1 error, got %d", rpt.TotalStats.ErrorCount)
	}
}

// A session id shorter than 8 characters must not panic when the reconstruction
// log line truncates it for display. Regression test for an unsafe sid[:8].
func TestShortSessionIDNoPanic(t *testing.T) {
	cases := []struct {
		name string
		s    string
		want string
	}{
		{"shorter than 8", "x", "x"},
		{"exactly 8", "abcdefgh", "abcdefgh"},
		{"longer than 8", "abcdefghij", "abcdefgh"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shortID(tc.s); got != tc.want {
				t.Errorf("shortID(%q) = %q, want %q", tc.s, got, tc.want)
			}
		})
	}
}

// Summarizing a record whose session id resolves to a <8-char string (here a
// Bedrock metadata.user_id ending in "_session_x") must not panic.
func TestSummarizeShortSessionID(t *testing.T) {
	ts := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	rec := makeLog("claude-3", "arn:aws:iam::123:user/alice", ts, 10, 20)
	rec.Input.JSON = json.RawMessage(`{"metadata":{"user_id":"user_h_account__session_x"},"messages":[{"role":"user","content":"hi"}]}`)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Summarize panicked on short session id: %v", r)
		}
	}()

	rpt := Summarize([]model.Record{rec})
	if len(rpt.Days) != 1 {
		t.Fatalf("expected 1 day, got %d", len(rpt.Days))
	}
	conv := rpt.Days[0].Conversations[0]
	if conv.SessionID != "x" {
		t.Errorf("SessionID = %q, want x", conv.SessionID)
	}
}

// Vertex records flow through Summarize end-to-end. Like every provider, the
// turn-by-turn Detail is produced for session-grouped conversations; here both
// records carry a _session_ marker so they are session-grouped. The Gemini
// conversation gets a reconstructed Detail; the Claude-on-Vertex conversation
// still appears in the stats and conversation list but has no Detail (the
// reconstructor defers it to #4).
func TestSummarizeVertexGeminiAndDeferredClaude(t *testing.T) {
	base := time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC)

	gemReq := `{"systemInstruction":{"parts":[{"text":"_session_gem-1111"}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	gemResp := `{"candidates":[{"content":{"role":"model","parts":[{"text":"hello"}]}}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2}}`

	vertexRecord := func(modelID, input, output string, in, out int64) model.Record {
		return model.Record{
			Provider:  "vertex",
			Timestamp: base,
			ModelID:   modelID,
			RequestID: modelID + "-1",
			Operation: "generateContent",
			Identity:  model.Identity{Extra: map[string]string{"project": "p"}},
			Input:     model.Body{JSON: json.RawMessage(input), TokenCount: in},
			Output:    model.Body{JSON: json.RawMessage(output), TokenCount: out},
		}
	}

	claudeReq := `{"messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"u_account__session_claude-2222"}}`

	logs := []model.Record{
		vertexRecord("gemini-2.0-flash", gemReq, gemResp, 5, 2),
		vertexRecord("claude-3-5-sonnet@20240620", claudeReq, `{}`, 7, 3),
	}

	rpt := Summarize(logs)

	if rpt.TotalStats.InvocationCount != 2 {
		t.Errorf("InvocationCount = %d, want 2", rpt.TotalStats.InvocationCount)
	}
	if rpt.TotalStats.InputTokens != 12 || rpt.TotalStats.OutputTokens != 5 {
		t.Errorf("tokens = %d/%d, want 12/5 (both models counted)", rpt.TotalStats.InputTokens, rpt.TotalStats.OutputTokens)
	}
	if len(rpt.Days) != 1 {
		t.Fatalf("days = %d, want 1", len(rpt.Days))
	}

	var gemini, claude *model.ConversationSummary
	for i := range rpt.Days[0].Conversations {
		c := &rpt.Days[0].Conversations[i]
		switch c.ModelID {
		case "gemini-2.0-flash":
			gemini = c
		case "claude-3-5-sonnet@20240620":
			claude = c
		}
	}
	if gemini == nil || claude == nil {
		t.Fatalf("expected both a gemini and a claude conversation, got %+v", rpt.Days[0].Conversations)
	}
	if gemini.Detail == nil || len(gemini.Detail.Turns) == 0 {
		t.Errorf("gemini conversation should have a reconstructed Detail, got %+v", gemini.Detail)
	}
	if claude.Detail != nil {
		t.Errorf("claude-on-vertex conversation Detail = %+v, want nil (deferred to #4)", claude.Detail)
	}
}

// planCountingSource is a BodySource double that counts loads, for asserting
// grouping does no body IO.
type planCountingSource struct {
	data  json.RawMessage
	loads *int
}

func (s planCountingSource) Load() (json.RawMessage, error) {
	*s.loads++
	return s.data, nil
}

// TestNewPlanMatchesSummarize asserts the Plan skeleton carries the same stats
// and day structure as the fully materialized report, and that streaming the
// conversations (without retention) yields them in the report's display order.
func TestNewPlanMatchesSummarize(t *testing.T) {
	base := time.Date(2024, 3, 1, 9, 0, 0, 0, time.UTC)
	input := `{"metadata":{"user_id":"user_h_account__session_planned"},"messages":[{"role":"user","content":"hi"}]}`
	records := []model.Record{
		makeLog("model-a", "arn:a", base, 10, 5),
		makeLog("model-a", "arn:a", base.Add(20*time.Minute), 20, 10), // beyond gap: second conversation
		makeLog("model-b", "arn:b", base.Add(24*time.Hour), 30, 15),   // second day
	}
	records[0].Input.JSON = json.RawMessage(input) // session-grouped conversation

	full := Summarize(records)
	plan := NewPlan(records)
	skeleton := plan.Report()

	if skeleton.TotalStats.InvocationCount != full.TotalStats.InvocationCount ||
		skeleton.TotalStats.InputTokens != full.TotalStats.InputTokens ||
		skeleton.TotalStats.OutputTokens != full.TotalStats.OutputTokens {
		t.Errorf("skeleton totals = %+v, want %+v", skeleton.TotalStats, full.TotalStats)
	}
	if len(skeleton.Days) != len(full.Days) {
		t.Fatalf("skeleton days = %d, want %d", len(skeleton.Days), len(full.Days))
	}
	for i := range skeleton.Days {
		if !skeleton.Days[i].Date.Equal(full.Days[i].Date) {
			t.Errorf("day[%d] date = %v, want %v", i, skeleton.Days[i].Date, full.Days[i].Date)
		}
		if len(skeleton.Days[i].Conversations) != 0 {
			t.Errorf("day[%d] skeleton has %d conversations before drain, want 0", i, len(skeleton.Days[i].Conversations))
		}
	}

	// Streaming yield order must match the materialized report's display order.
	var want []string
	for _, day := range full.Days {
		for _, cs := range day.Conversations {
			want = append(want, cs.ID)
		}
	}
	var got []string
	for cs := range plan.Conversations(MaterializeOptions{}) {
		got = append(got, cs.ID)
	}
	if len(got) != len(want) {
		t.Fatalf("streamed %d conversations, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("stream order[%d] = %s, want %s", i, got[i], want[i])
		}
	}
	// The plan's report must remain a skeleton after a non-retaining drain.
	for i := range skeleton.Days {
		if len(skeleton.Days[i].Conversations) != 0 {
			t.Errorf("day[%d] gained %d conversations from non-retaining drain", i, len(skeleton.Days[i].Conversations))
		}
	}
}

// TestNewPlanNoBodyLoads asserts grouping performs zero body loads when the
// fetcher pre-extracted the session id (the lazy-body contract).
func TestNewPlanNoBodyLoads(t *testing.T) {
	base := time.Date(2024, 3, 1, 9, 0, 0, 0, time.UTC)
	var loads int
	rec := makeLog("model-a", "arn:a", base, 10, 5)
	rec.SessionID = "sess-lazy"
	rec.Input = model.Body{Source: planCountingSource{data: json.RawMessage(`{"messages":[]}`), loads: &loads}}

	plan := NewPlan([]model.Record{rec})
	if loads != 0 {
		t.Errorf("grouping loaded %d bodies, want 0", loads)
	}
	if len(plan.convs) != 1 || plan.convs[0].sessionID != "sess-lazy" {
		t.Fatalf("plan convs = %+v, want one session-grouped conversation", plan.convs)
	}
}
