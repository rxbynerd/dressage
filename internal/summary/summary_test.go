package summary

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rubynerd/dressage/internal/model"
)

func makeLog(modelID, arn string, ts time.Time, inputTokens, outputTokens int64) model.InvocationLog {
	return model.InvocationLog{
		Timestamp:  ts,
		ModelID:    modelID,
		RequestID:  ts.Format(time.RFC3339Nano),
		Operation:  "InvokeModel",
		Status:     "200",
		Identity:   model.Identity{ARN: arn},
		Input:      model.InvocationInput{InputTokenCount: inputTokens},
		Output:     model.InvocationOutput{OutputTokenCount: outputTokens},
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
	logs := []model.InvocationLog{
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

// Two invocations within the gap threshold should form a single conversation.
func TestConversationGroupingWithinGap(t *testing.T) {
	base := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	arn := "arn:aws:iam::123:user/alice"
	mdl := "claude-3"

	logs := []model.InvocationLog{
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

	logs := []model.InvocationLog{
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

	logs := []model.InvocationLog{
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

	logs := []model.InvocationLog{
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

	logs := []model.InvocationLog{
		makeLog(mdl, arn, base.Add(-time.Minute), 100, 50),
		errLog,
	}

	rpt := Summarize(logs)

	if rpt.TotalStats.ErrorCount != 1 {
		t.Errorf("expected 1 error, got %d", rpt.TotalStats.ErrorCount)
	}
}

func TestPrettyJSON(t *testing.T) {
	cases := []struct {
		name  string
		input json.RawMessage
		want  string
	}{
		{"nil", nil, ""},
		{"empty", json.RawMessage{}, ""},
		{"object", json.RawMessage(`{"a":1}`), "{\n  \"a\": 1\n}"},
		{"invalid", json.RawMessage(`not-json`), "not-json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := prettyJSON(tc.input)
			if got != tc.want {
				t.Errorf("prettyJSON(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
