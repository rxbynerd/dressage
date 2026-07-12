package ir_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/parquet-go/parquet-go"

	"github.com/rxbynerd/dressage/internal/ir"
	"github.com/rxbynerd/dressage/internal/model"
)

// factsReport is a claude-flavored fixture exercising every facts column:
// correlation metadata, a sidechain thread, an errored invocation, and
// identity extras.
func factsReport() *model.Report {
	t0 := time.Date(2024, 2, 1, 9, 0, 0, 0, time.UTC)
	cs := model.ConversationSummary{
		ID:        "conv-20240201-0",
		SessionID: "sess-facts",
		Provider:  "claude",
		ModelID:   "claude-test",
		StartTime: t0,
		EndTime:   t0.Add(30 * time.Second),
		Invocations: []model.Invocation{
			{
				Timestamp:    t0,
				RequestID:    "req_A",
				ModelID:      "claude-test",
				Operation:    "messages",
				Status:       "200",
				StopReason:   "end_turn",
				LatencyMs:    0,
				InputTokens:  10,
				OutputTokens: 100,
				Correlation: model.Correlation{
					MessageID:   "msg_1",
					ThreadID:    "main-root",
					RequestUUID: "main-root",
					NumMessages: 1,
				},
				FullIdentity: model.Identity{
					Principal: "acct-1",
					Extra:     map[string]string{"session_id": "sess-facts", "device_id": "dev-1"},
				},
				Input:  model.Body{TokenCount: 10, CacheRead: 5, CacheWrite: 2},
				Output: model.Body{TokenCount: 100},
			},
			{
				Timestamp:    t0.Add(10 * time.Second),
				RequestID:    "req_S",
				ModelID:      "claude-test",
				Operation:    "messages",
				Status:       "200",
				StopReason:   "end_turn",
				InputTokens:  99,
				OutputTokens: 999,
				Correlation: model.Correlation{
					MessageID:   "msg_s1",
					ThreadID:    "sub-root",
					RequestUUID: "sub-root",
					NumMessages: 1,
				},
				FullIdentity: model.Identity{Principal: "acct-1"},
				Input:        model.Body{TokenCount: 99},
				Output:       model.Body{TokenCount: 999},
			},
			{
				Timestamp: t0.Add(20 * time.Second),
				RequestID: "req_ERR",
				ModelID:   "claude-test",
				Operation: "messages",
				Status:    "429",
				ErrorCode: "rate_limited",
				Correlation: model.Correlation{
					PrevMessageID: "msg_1",
					ThreadID:      "main-root",
					RequestUUID:   "err-req",
					NumMessages:   3,
				},
				FullIdentity: model.Identity{Principal: "acct-1"},
			},
		},
	}
	return &model.Report{
		GeneratedAt: fixedGeneratedAt,
		TotalStats:  model.Stats{InvocationCount: 3},
		Days: []model.DaySummary{{
			Date:          t0.Truncate(24 * time.Hour),
			Conversations: []model.ConversationSummary{cs},
		}},
	}
}

// TestGoldenFacts locks the facts table's schema and row content via a
// decoded-row JSON golden (Parquet bytes are not stable across library
// versions, so the golden is the decoded content, not the file).
func TestGoldenFacts(t *testing.T) {
	tmp := t.TempDir()
	if err := ir.Export(factsReport(), tmp, fixedSource, ir.ExportOptions{}); err != nil {
		t.Fatalf("export: %v", err)
	}

	rows, err := parquet.ReadFile[ir.FactRow](filepath.Join(tmp, ir.FactsFile))
	if err != nil {
		t.Fatalf("read facts parquet: %v", err)
	}
	got, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		t.Fatalf("marshal rows: %v", err)
	}
	got = append(got, '\n')

	goldenPath := filepath.Join("testdata", "golden_facts.json")
	if *updateGolden {
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("wrote %s", goldenPath)
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("decoded facts rows differ from golden.\ngot:\n%s\nwant:\n%s\nIf intentional, re-run with -update.", got, want)
	}

	// Manifest points at the table.
	var manifest ir.Manifest
	readJSON(t, filepath.Join(tmp, "manifest.json"), &manifest)
	if manifest.Files.Facts != ir.FactsFile {
		t.Errorf("manifest files.facts = %q, want %q", manifest.Files.Facts, ir.FactsFile)
	}
}
