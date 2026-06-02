package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

func TestRunReportFormatHTMLOnly(t *testing.T) {
	tmp := t.TempDir()
	out := filepath.Join(tmp, "report.html")
	common := &commonFlags{output: out, format: "html"}

	if err := runReport(context.Background(), fakeFetcher{smokeRecords()}, "Smoke Report", "bedrock", common); err != nil {
		t.Fatalf("runReport: %v", err)
	}

	if _, err := os.Stat(out); err != nil {
		t.Errorf("HTML report not written for --format html: %v", err)
	}
	// The default format must NOT create the IR directory.
	if _, err := os.Stat(filepath.Join(tmp, "report.ir")); !os.IsNotExist(err) {
		t.Errorf("IR dir should not exist for --format html, stat err = %v", err)
	}
}

func TestCommandStringRedactsSensitiveFlags(t *testing.T) {
	orig := os.Args
	t.Cleanup(func() { os.Args = orig })

	os.Args = []string{
		"dressage", "vertex",
		"--credentials", "/path/to/key.json",
		"--profile", "prod-profile",
		"--subscription=11111111-2222-3333-4444-555555555555",
		"--bucket", "my-public-bucket",
	}

	got := commandString()

	for _, secret := range []string{"/path/to/key.json", "prod-profile", "11111111-2222-3333-4444-555555555555"} {
		if strings.Contains(got, secret) {
			t.Errorf("command string leaks %q: %s", secret, got)
		}
	}
	if !strings.Contains(got, "<redacted>") {
		t.Errorf("command string has no redaction marker: %s", got)
	}
	// Flag names and non-sensitive values must survive.
	if !strings.Contains(got, "--credentials <redacted>") {
		t.Errorf("expected --credentials <redacted>, got: %s", got)
	}
	if !strings.Contains(got, "--subscription=<redacted>") {
		t.Errorf("expected --subscription=<redacted> (=form), got: %s", got)
	}
	if !strings.Contains(got, "--bucket my-public-bucket") {
		t.Errorf("non-sensitive flag value was dropped: %s", got)
	}
}
