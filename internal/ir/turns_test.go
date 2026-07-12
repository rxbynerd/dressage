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

// turnsReport is a fixture with a reconstructed main thread and one sidechain,
// exercising the turns table's content and metrics columns.
func turnsReport() *model.Report {
	t0 := time.Date(2024, 2, 1, 9, 0, 0, 0, time.UTC)
	cs := model.ConversationSummary{
		ID:        "conv-20240201-0",
		SessionID: "sess-turns",
		Provider:  "claude",
		ModelID:   "claude-test",
		StartTime: t0,
		EndTime:   t0.Add(30 * time.Second),
		Invocations: []model.Invocation{
			{Timestamp: t0, RequestID: "req_A", ModelID: "claude-test"},
		},
		Detail: &model.ConversationDetail{
			SessionID:    "sess-turns",
			SystemPrompt: "You are helpful.",
			Turns: []model.Turn{
				{Role: "user", Blocks: []model.ContentBlock{{Type: "text", Text: "do the task"}}},
				{
					Role: "assistant",
					Blocks: []model.ContentBlock{
						{Type: "text", Text: "dispatching a subagent"},
						{Type: "tool_use", ToolName: "Agent", ToolID: "toolu_1", ToolInput: `{"prompt": "explore"}`},
					},
					Metrics: &model.TurnMetrics{
						Timestamp:    t0,
						RequestID:    "req_A",
						ModelID:      "claude-test",
						InputTokens:  10,
						OutputTokens: 100,
						StopReason:   "tool_use",
					},
				},
			},
		},
		Sidechains: []model.Thread{{
			ID: "sub-root",
			Detail: &model.ConversationDetail{
				Turns: []model.Turn{
					{Role: "user", Blocks: []model.ContentBlock{{Type: "text", Text: "explore"}}},
					{
						Role:   "assistant",
						Blocks: []model.ContentBlock{{Type: "text", Text: "found it"}},
						Metrics: &model.TurnMetrics{
							Timestamp:    t0.Add(10 * time.Second),
							RequestID:    "req_S",
							ModelID:      "claude-test",
							InputTokens:  5,
							OutputTokens: 50,
							StopReason:   "end_turn",
						},
					},
				},
			},
		}},
	}
	return &model.Report{
		GeneratedAt: fixedGeneratedAt,
		TotalStats:  model.Stats{InvocationCount: 1},
		Days: []model.DaySummary{{
			Date:          t0.Truncate(24 * time.Hour),
			Conversations: []model.ConversationSummary{cs},
		}},
	}
}

// TestGoldenTurns locks the turns table's schema and row content — main
// thread and sidechain rows — via a decoded-row JSON golden.
func TestGoldenTurns(t *testing.T) {
	tmp := t.TempDir()
	if err := ir.Export(turnsReport(), tmp, fixedSource, ir.ExportOptions{}); err != nil {
		t.Fatalf("export: %v", err)
	}

	rows, err := parquet.ReadFile[ir.TurnRow](filepath.Join(tmp, ir.TurnsFile))
	if err != nil {
		t.Fatalf("read turns parquet: %v", err)
	}
	got, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		t.Fatalf("marshal rows: %v", err)
	}
	got = append(got, '\n')

	goldenPath := filepath.Join("testdata", "golden_turns.json")
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
		t.Errorf("decoded turns rows differ from golden.\ngot:\n%s\nwant:\n%s\nIf intentional, re-run with -update.", got, want)
	}

	var manifest ir.Manifest
	readJSON(t, filepath.Join(tmp, "manifest.json"), &manifest)
	if manifest.Files.Turns != ir.TurnsFile {
		t.Errorf("manifest files.turns = %q, want %q", manifest.Files.Turns, ir.TurnsFile)
	}

	// Sidechain rows are present and distinguishable.
	var side int
	for _, r := range rows {
		if r.ThreadID == "sub-root" {
			side++
		}
	}
	if side != 2 {
		t.Errorf("sidechain rows = %d, want 2", side)
	}
}
