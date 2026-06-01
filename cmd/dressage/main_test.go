package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rxbynerd/dressage/internal/model"
)

// fakeFetcher returns a canned set of records, ignoring the date window.
type fakeFetcher struct {
	records []model.Record
}

func (f fakeFetcher) Fetch(_ context.Context, _, _ time.Time) ([]model.Record, error) {
	return f.records, nil
}

// smokeRecords is one minimal reconstructable Bedrock conversation.
func smokeRecords() []model.Record {
	in := `{
		"metadata":{"user_id":"user_h_account__session_smoke-1"},
		"system":"You are helpful.",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[]
	}`
	out := `{"id":"m1","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"hello"}]}`
	return []model.Record{{
		Provider:  "bedrock",
		Timestamp: time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
		RequestID: "req-1",
		ModelID:   "claude-opus-4-6",
		Operation: "Converse",
		Status:    "200",
		Identity:  model.Identity{Principal: "arn:aws:iam::111:user/dev"},
		Input:     model.Body{JSON: json.RawMessage(in), ContentType: "application/json", TokenCount: 5},
		Output:    model.Body{JSON: json.RawMessage(out), ContentType: "application/json", TokenCount: 3},
	}}
}

func TestResolveIRDir(t *testing.T) {
	cases := []struct {
		name   string
		output string
		irDir  string
		want   string
	}{
		{"derived from html output", "report.html", "", "report.ir"},
		{"derived with path", filepath.Join("out", "march.html"), "", filepath.Join("out", "march.ir")},
		{"derived without extension", "report", "", "report.ir"},
		{"explicit ir-dir wins", "report.html", "custom.ir", "custom.ir"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolveIRDir(&commonFlags{output: c.output, irDir: c.irDir})
			if got != c.want {
				t.Errorf("resolveIRDir(output=%q, irDir=%q) = %q, want %q", c.output, c.irDir, got, c.want)
			}
		})
	}
}

func TestRunReportFormatBothWritesHTMLAndIR(t *testing.T) {
	tmp := t.TempDir()
	out := filepath.Join(tmp, "report.html")
	common := &commonFlags{output: out, format: "both"}

	err := runReport(context.Background(), fakeFetcher{smokeRecords()}, "Smoke Report", "bedrock", common)
	if err != nil {
		t.Fatalf("runReport: %v", err)
	}

	// HTML report exists.
	if _, err := os.Stat(out); err != nil {
		t.Errorf("HTML report not written: %v", err)
	}

	// IR directory + manifest + one conversation file exist.
	irDir := filepath.Join(tmp, "report.ir")
	manifestPath := filepath.Join(irDir, "manifest.json")
	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatalf("manifest not written: %v", err)
	}
	b, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest struct {
		Source struct {
			Provider string `json:"provider"`
		} `json:"source"`
		Conversations []struct {
			File string `json:"file"`
		} `json:"conversations"`
	}
	if err := json.Unmarshal(b, &manifest); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	if manifest.Source.Provider != "bedrock" {
		t.Errorf("manifest source provider = %q, want bedrock", manifest.Source.Provider)
	}
	if len(manifest.Conversations) != 1 {
		t.Fatalf("manifest conversations = %d, want 1", len(manifest.Conversations))
	}
	convPath := filepath.Join(irDir, filepath.FromSlash(manifest.Conversations[0].File))
	if _, err := os.Stat(convPath); err != nil {
		t.Errorf("conversation file not written: %v", err)
	}
}

func TestRunReportFormatIROnlySkipsHTML(t *testing.T) {
	tmp := t.TempDir()
	out := filepath.Join(tmp, "report.html")
	common := &commonFlags{output: out, format: "ir"}

	if err := runReport(context.Background(), fakeFetcher{smokeRecords()}, "Smoke Report", "bedrock", common); err != nil {
		t.Fatalf("runReport: %v", err)
	}

	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("HTML report should not exist for --format ir, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "report.ir", "manifest.json")); err != nil {
		t.Errorf("IR manifest not written for --format ir: %v", err)
	}
}

func TestRunReportRejectsInvalidFormat(t *testing.T) {
	common := &commonFlags{output: "report.html", format: "yaml"}
	err := runReport(context.Background(), fakeFetcher{smokeRecords()}, "Smoke Report", "bedrock", common)
	if err == nil {
		t.Fatal("expected an error for invalid --format, got nil")
	}
}
