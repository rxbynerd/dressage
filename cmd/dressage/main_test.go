package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rxbynerd/dressage/internal/ir"
	"github.com/rxbynerd/dressage/internal/model"
	"github.com/rxbynerd/dressage/internal/serve"
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

// TestRunReportWritesIR asserts an ingestion run writes the IR directory — a
// manifest plus one conversation file — as its sole output, with the provider
// recorded in the manifest source.
func TestRunReportWritesIR(t *testing.T) {
	tmp := t.TempDir()
	irDir := filepath.Join(tmp, "out.ir")
	common := &commonFlags{out: irDir}

	if err := runReport(context.Background(), fakeFetcher{smokeRecords()}, "bedrock", common); err != nil {
		t.Fatalf("runReport: %v", err)
	}

	b, err := os.ReadFile(filepath.Join(irDir, "manifest.json"))
	if err != nil {
		t.Fatalf("manifest not written: %v", err)
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

	// No HTML report is produced any more.
	if _, err := os.Stat(filepath.Join(tmp, "report.html")); !os.IsNotExist(err) {
		t.Errorf("unexpected HTML output, stat err = %v", err)
	}
}

// TestServeCommandServesExportedIR closes the loop: an IR produced by an
// ingestion run opens cleanly and the serve handler renders its index.
func TestServeCommandServesExportedIR(t *testing.T) {
	tmp := t.TempDir()
	irDir := filepath.Join(tmp, "out.ir")
	if err := runReport(context.Background(), fakeFetcher{smokeRecords()}, "bedrock", &commonFlags{out: irDir}); err != nil {
		t.Fatalf("runReport: %v", err)
	}

	reader, err := ir.OpenDir(irDir)
	if err != nil {
		t.Fatalf("open IR: %v", err)
	}
	h := serve.New(reader).Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "bedrock") {
		t.Errorf("index page missing provider name")
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
