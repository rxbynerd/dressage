package ir_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rxbynerd/dressage/internal/ir"
	"github.com/rxbynerd/dressage/internal/model"
	"github.com/rxbynerd/dressage/internal/summary"
)

// updateGolden regenerates the golden IR fixtures instead of asserting against
// them. Run: go test ./internal/ir/ -run Golden -update
var updateGolden = flag.Bool("update", false, "update golden IR fixtures")

// fixedGeneratedAt pins the report's GeneratedAt so the emitted manifest is
// deterministic across runs.
var fixedGeneratedAt = time.Date(2024, 1, 20, 12, 0, 0, 0, time.UTC)

// fixedSource is the canned run metadata used for the golden manifest.
var fixedSource = ir.SourceInfo{
	Provider: "bedrock",
	Command:  "dressage bedrock --bucket my-logs --format ir",
	DateRange: ir.ManifestDateRng{
		Start: "2024-01-15",
		End:   "2024-01-15",
	},
}

// TestGoldenIRExport feeds a canned *model.Report covering all four provider
// situations (Bedrock/Anthropic, Azure/OpenAI, Vertex/Gemini, and a deferred
// provider) through ir.Export into a temp dir and compares every emitted file
// byte-for-byte against committed goldens. It locks the IR schema shape,
// determinism, and stable-id derivation.
func TestGoldenIRExport(t *testing.T) {
	rpt := goldenReport()

	goldenDir := filepath.Join("testdata", "golden_ir")
	if *updateGolden {
		if err := os.RemoveAll(goldenDir); err != nil {
			t.Fatalf("clean golden dir: %v", err)
		}
		if err := ir.Export(rpt, goldenDir, fixedSource, ir.ExportOptions{}); err != nil {
			t.Fatalf("export (update): %v", err)
		}
		t.Logf("wrote golden IR fixtures under %s", goldenDir)
		return
	}

	tmp := t.TempDir()
	if err := ir.Export(rpt, tmp, fixedSource, ir.ExportOptions{}); err != nil {
		t.Fatalf("export: %v", err)
	}

	wantFiles := collectFiles(t, goldenDir)
	gotFiles := collectFiles(t, tmp)

	if len(wantFiles) != len(gotFiles) {
		t.Fatalf("file set differs: emitted %v, golden %v", keys(gotFiles), keys(wantFiles))
	}
	for rel, want := range wantFiles {
		got, ok := gotFiles[rel]
		if !ok {
			t.Errorf("missing emitted file %s", rel)
			continue
		}
		if !bytes.Equal(want, got) {
			t.Errorf("file %s differs from golden.\n"+
				"If this change is intentional, re-run with -update and review the diff.", rel)
		}
	}
}

// TestIRExportRoundTrip exports the canned report and unmarshals each emitted
// file back into the ir types, asserting the IR is valid, self-describing JSON.
func TestIRExportRoundTrip(t *testing.T) {
	rpt := goldenReport()
	tmp := t.TempDir()
	if err := ir.Export(rpt, tmp, fixedSource, ir.ExportOptions{}); err != nil {
		t.Fatalf("export: %v", err)
	}

	manifestBytes, err := os.ReadFile(filepath.Join(tmp, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest ir.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	if manifest.SchemaVersion != ir.SchemaVersion {
		t.Errorf("manifest schema_version = %q, want %q", manifest.SchemaVersion, ir.SchemaVersion)
	}
	if manifest.Totals.Conversations != len(manifest.Conversations) {
		t.Errorf("totals.conversations = %d, but %d index entries", manifest.Totals.Conversations, len(manifest.Conversations))
	}

	for _, entry := range manifest.Conversations {
		b, err := os.ReadFile(filepath.Join(tmp, filepath.FromSlash(entry.File)))
		if err != nil {
			t.Fatalf("read conversation %s: %v", entry.File, err)
		}
		var conv ir.ConversationIR
		if err := json.Unmarshal(b, &conv); err != nil {
			t.Fatalf("unmarshal conversation %s: %v", entry.File, err)
		}
		if conv.ID != entry.ID {
			t.Errorf("conversation %s id = %q, want %q", entry.File, conv.ID, entry.ID)
		}
		if conv.SchemaVersion != ir.SchemaVersion {
			t.Errorf("conversation %s schema_version = %q, want %q", entry.File, conv.SchemaVersion, ir.SchemaVersion)
		}
		// invocations are always populated; reconstructed conversations carry a
		// non-nil conversation view, deferred ones a nil one.
		if len(conv.Invocations) == 0 {
			t.Errorf("conversation %s has no invocations", entry.File)
		}
		if entry.Reconstructed && conv.Conversation == nil {
			t.Errorf("conversation %s marked reconstructed but conversation is null", entry.File)
		}
		if !entry.Reconstructed && conv.Conversation != nil {
			t.Errorf("conversation %s marked deferred but conversation is non-null", entry.File)
		}
	}
}

// TestIRExportDeferredProvider asserts the deferred provider conversation
// exports with a null conversation but populated invocations.
func TestIRExportDeferredProvider(t *testing.T) {
	rpt := goldenReport()
	tmp := t.TempDir()
	if err := ir.Export(rpt, tmp, fixedSource, ir.ExportOptions{}); err != nil {
		t.Fatalf("export: %v", err)
	}

	var manifest ir.Manifest
	readJSON(t, filepath.Join(tmp, "manifest.json"), &manifest)

	var deferred ir.ManifestEntry
	var found bool
	for _, e := range manifest.Conversations {
		if e.Provider == "vertex" && !e.Reconstructed {
			deferred = e
			found = true
		}
	}
	if !found {
		t.Fatal("no deferred (vertex, non-reconstructed) conversation in manifest")
	}

	// Close the loop on the manifest index fields, not just the file content.
	if deferred.Reconstructed {
		t.Error("manifest entry.Reconstructed = true, want false for deferred provider")
	}
	if deferred.TurnCount != 0 {
		t.Errorf("manifest entry.TurnCount = %d, want 0 for deferred provider", deferred.TurnCount)
	}

	raw, err := os.ReadFile(filepath.Join(tmp, filepath.FromSlash(deferred.File)))
	if err != nil {
		t.Fatalf("read deferred conversation: %v", err)
	}
	// Assert the raw JSON carries an explicit null conversation.
	if !bytes.Contains(raw, []byte(`"conversation": null`)) {
		t.Errorf("deferred conversation file lacks \"conversation\": null:\n%s", raw)
	}

	var conv ir.ConversationIR
	if err := json.Unmarshal(raw, &conv); err != nil {
		t.Fatalf("unmarshal deferred conversation: %v", err)
	}
	if conv.Conversation != nil {
		t.Error("deferred conversation has non-nil conversation view")
	}
	if len(conv.Invocations) == 0 {
		t.Error("deferred conversation has no invocations (must still be populated)")
	}
}

// TestIRExportFidelity asserts that a >200-char tool description survives
// untruncated with its input_schema intact, and that a Gemini media part is
// exported as a media block.
func TestIRExportFidelity(t *testing.T) {
	rpt := goldenReport()
	tmp := t.TempDir()
	if err := ir.Export(rpt, tmp, fixedSource, ir.ExportOptions{}); err != nil {
		t.Fatalf("export: %v", err)
	}

	var manifest ir.Manifest
	readJSON(t, filepath.Join(tmp, "manifest.json"), &manifest)

	var sawLongTool, sawSchema, sawMedia bool
	for _, e := range manifest.Conversations {
		var conv ir.ConversationIR
		readJSON(t, filepath.Join(tmp, filepath.FromSlash(e.File)), &conv)
		if conv.Conversation == nil {
			continue
		}
		for _, tool := range conv.Conversation.Tools {
			if len(tool.Description) > 200 {
				sawLongTool = true
			}
			if len(tool.InputSchema) > 0 {
				sawSchema = true
			}
		}
		for _, turn := range conv.Conversation.Turns {
			for _, b := range turn.Blocks {
				if b.Type == "media" {
					sawMedia = true
				}
			}
		}
	}

	if !sawLongTool {
		t.Error("no tool with a >200-char description survived export")
	}
	if !sawSchema {
		t.Error("no tool input_schema survived export")
	}
	if !sawMedia {
		t.Error("no media block was produced from the Gemini media part")
	}
}

// TestIRExportDeterministic asserts two exports of the same report into
// different dirs produce byte-identical files.
func TestIRExportDeterministic(t *testing.T) {
	rpt := goldenReport()

	dirA, dirB := t.TempDir(), t.TempDir()
	if err := ir.Export(rpt, dirA, fixedSource, ir.ExportOptions{}); err != nil {
		t.Fatalf("export A: %v", err)
	}
	if err := ir.Export(rpt, dirB, fixedSource, ir.ExportOptions{}); err != nil {
		t.Fatalf("export B: %v", err)
	}

	a := collectFiles(t, dirA)
	b := collectFiles(t, dirB)
	if len(a) != len(b) {
		t.Fatalf("file counts differ: %d vs %d", len(a), len(b))
	}
	for rel, ba := range a {
		if !bytes.Equal(ba, b[rel]) {
			t.Errorf("file %s not deterministic across runs", rel)
		}
	}
}

// reportWith wraps a single conversation summary in a one-day *model.Report.
func reportWith(cs model.ConversationSummary) *model.Report {
	return &model.Report{
		GeneratedAt: fixedGeneratedAt,
		Days:        []model.DaySummary{{Conversations: []model.ConversationSummary{cs}}},
	}
}

// TestExportFilesystemHostileSessionID asserts a session id with path-significant
// characters cannot escape the conversations directory or break the write: the
// id field stays raw, the file lands flat inside conversations/, and the
// manifest `file` matches what was actually written.
func TestExportFilesystemHostileSessionID(t *testing.T) {
	cs := model.ConversationSummary{
		ID:           "conv-20240115-0",
		SessionID:    "prod/service:v2",
		Provider:     "bedrock",
		ModelID:      "claude-opus-4-6",
		Identity:     "arn:aws:iam::111:user/dev",
		StartTime:    fixedGeneratedAt,
		EndTime:      fixedGeneratedAt,
		MessageCount: 1,
		Invocations: []model.Invocation{
			{RequestID: "req-1", ModelID: "claude-opus-4-6", Input: model.Body{JSON: json.RawMessage(`{"a":1}`)}},
		},
	}
	tmp := t.TempDir()
	if err := ir.Export(reportWith(cs), tmp, fixedSource, ir.ExportOptions{}); err != nil {
		t.Fatalf("export: %v", err)
	}

	// (b) No file landed outside tmp/conversations/. Every emitted file must be
	// the manifest or live under conversations/.
	for rel := range collectFiles(t, tmp) {
		if rel == "manifest.json" {
			continue
		}
		if dir := filepath.Dir(rel); dir != "conversations" {
			t.Errorf("file %q escaped conversations/ (dir %q)", rel, dir)
		}
	}

	var manifest ir.Manifest
	readJSON(t, filepath.Join(tmp, "manifest.json"), &manifest)
	if len(manifest.Conversations) != 1 {
		t.Fatalf("conversations = %d, want 1", len(manifest.Conversations))
	}
	entry := manifest.Conversations[0]

	// (c) The filename has no path separator.
	if strings.ContainsAny(filepath.Base(entry.File), `/\`) {
		t.Errorf("filename contains a separator: %q", entry.File)
	}
	if entry.File != "conversations/prod_service_v2.json" {
		t.Errorf("manifest file = %q, want conversations/prod_service_v2.json", entry.File)
	}

	// (e) The manifest file path matches the actual file on disk.
	if _, err := os.Stat(filepath.Join(tmp, filepath.FromSlash(entry.File))); err != nil {
		t.Errorf("manifest file does not match disk: %v", err)
	}

	// (d) The written JSON's top-level id is the unmodified raw session id, and
	// (a) Export returned nil (asserted above).
	var conv ir.ConversationIR
	readJSON(t, filepath.Join(tmp, filepath.FromSlash(entry.File)), &conv)
	if conv.ID != "prod/service:v2" {
		t.Errorf("conversation id = %q, want the raw session id unmodified", conv.ID)
	}
	if entry.ID != "prod/service:v2" {
		t.Errorf("manifest entry id = %q, want the raw session id unmodified", entry.ID)
	}
}

// TestIRExportEmptyReport asserts a zero-conversation report writes a valid
// empty manifest and creates conversations/ without error.
func TestIRExportEmptyReport(t *testing.T) {
	rpt := &model.Report{GeneratedAt: fixedGeneratedAt}
	tmp := t.TempDir()
	if err := ir.Export(rpt, tmp, fixedSource, ir.ExportOptions{}); err != nil {
		t.Fatalf("export empty report: %v", err)
	}

	if info, err := os.Stat(filepath.Join(tmp, "conversations")); err != nil || !info.IsDir() {
		t.Errorf("conversations/ not created: err=%v", err)
	}

	var manifest ir.Manifest
	readJSON(t, filepath.Join(tmp, "manifest.json"), &manifest)
	if manifest.SchemaVersion != ir.SchemaVersion {
		t.Errorf("schema_version = %q, want %q", manifest.SchemaVersion, ir.SchemaVersion)
	}
	if len(manifest.Conversations) != 0 {
		t.Errorf("conversations = %d, want 0", len(manifest.Conversations))
	}
	if manifest.Totals.Conversations != 0 {
		t.Errorf("totals.conversations = %d, want 0", manifest.Totals.Conversations)
	}
	// The conversations array must serialize as [] (an empty JSON array), not null.
	raw, err := os.ReadFile(filepath.Join(tmp, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"conversations": []`)) {
		t.Errorf("empty manifest should carry \"conversations\": [], got:\n%s", raw)
	}
}

// TestIRExportNonJSONToolInputRoundTrips asserts a tool_use block whose
// ToolInput is not valid JSON survives Export -> disk -> Unmarshal as a JSON
// string (not null and not invalid JSON).
func TestIRExportNonJSONToolInputRoundTrips(t *testing.T) {
	cs := model.ConversationSummary{
		ID:           "conv-20240115-0",
		SessionID:    "tool-input-sess",
		Provider:     "bedrock",
		ModelID:      "claude-opus-4-6",
		StartTime:    fixedGeneratedAt,
		EndTime:      fixedGeneratedAt,
		MessageCount: 1,
		Invocations: []model.Invocation{
			{RequestID: "req-1", ModelID: "claude-opus-4-6", Input: model.Body{JSON: json.RawMessage(`{}`)}},
		},
		Detail: &model.ConversationDetail{
			Turns: []model.Turn{{
				Role: "assistant",
				Blocks: []model.ContentBlock{{
					Type:      "tool_use",
					ToolID:    "toolu_1",
					ToolName:  "Bash",
					ToolInput: "not json at all",
				}},
			}},
		},
	}
	tmp := t.TempDir()
	if err := ir.Export(reportWith(cs), tmp, fixedSource, ir.ExportOptions{}); err != nil {
		t.Fatalf("export: %v", err)
	}

	var manifest ir.Manifest
	readJSON(t, filepath.Join(tmp, "manifest.json"), &manifest)
	if len(manifest.Conversations) != 1 {
		t.Fatalf("conversations = %d, want 1", len(manifest.Conversations))
	}

	// The file is valid JSON (readJSON would fail otherwise) and the tool_input
	// round-trips to the original string.
	var conv ir.ConversationIR
	readJSON(t, filepath.Join(tmp, filepath.FromSlash(manifest.Conversations[0].File)), &conv)
	if conv.Conversation == nil || len(conv.Conversation.Turns) != 1 {
		t.Fatalf("unexpected conversation shape: %+v", conv.Conversation)
	}
	block := conv.Conversation.Turns[0].Blocks[0]
	if block.ToolInput == nil {
		t.Fatal("tool_input is null, want a JSON string")
	}
	var s string
	if err := json.Unmarshal(block.ToolInput, &s); err != nil {
		t.Fatalf("tool_input is not a JSON string: %v", err)
	}
	if s != "not json at all" {
		t.Errorf("tool_input = %q, want the original string", s)
	}
}

func TestFormatDate(t *testing.T) {
	if got := ir.FormatDate(time.Time{}); got != "" {
		t.Errorf("zero time = %q, want empty", got)
	}
	want := "2026-06-01"
	if got := ir.FormatDate(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)); got != want {
		t.Errorf("date = %q, want %q", got, want)
	}
}

// goldenReport builds a deterministic *model.Report covering all four provider
// situations by running canned records through summary.Summarize, then pinning
// GeneratedAt. Using the real pipeline keeps the fixture honest: the same
// grouping, reconstruction, and stable-id logic the CLI uses produces the IR.
func goldenReport() *model.Report {
	records := goldenRecords()
	rpt := summary.Summarize(records)
	rpt.GeneratedAt = fixedGeneratedAt
	rpt.Title = "Golden IR Report"
	return rpt
}

// goldenRecords returns canned invocation records: a reconstructed Bedrock
// (Anthropic) conversation with a long tool description + input schema, an Azure
// (OpenAI) conversation, a Vertex (Gemini) conversation with a media part, and a
// deferred Claude-on-Vertex conversation.
func goldenRecords() []model.Record {
	base := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	longDesc := repeat("x", 250)

	// Bedrock / Anthropic, session-grouped, with a long-description tool that
	// carries an input schema.
	bedrockIn := `{
		"metadata":{"user_id":"user_hash_account__session_bedrock-sess-1"},
		"system":"You are a coding assistant.",
		"messages":[{"role":"user","content":"List the files."}],
		"tools":[{"name":"Bash","description":"` + longDesc + `","input_schema":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}]
	}`
	bedrockOut := `{"id":"msg_b","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"Here are the files."}]}`

	// Azure / OpenAI, session-grouped.
	azureIn := `{
		"user":"user_hash__session_azure-sess-1",
		"messages":[
			{"role":"system","content":"You are concise."},
			{"role":"user","content":"What is 2+2?"}
		],
		"tools":[{"type":"function","function":{"name":"calc","description":"does math","parameters":{"type":"object","properties":{"expr":{"type":"string"}}}}}]
	}`
	azureOut := `{"choices":[{"message":{"role":"assistant","content":"4"},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":1}}`

	// Vertex / Gemini, session-grouped, with an inline media part.
	geminiIn := `{
		"systemInstruction":{"parts":[{"text":"You are vision-capable._session_gemini-sess-1"}]},
		"contents":[{"role":"user","parts":[
			{"inlineData":{"mimeType":"image/png","data":"aGVsbG8="}},
			{"text":"What is in this image?"}
		]}],
		"tools":[{"functionDeclarations":[{"name":"describe","description":"describes an image","parameters":{"type":"object"}}]}]
	}`
	geminiOut := `{"candidates":[{"content":{"role":"model","parts":[{"text":"A greeting."}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":15,"candidatesTokenCount":3}}`

	// Deferred Claude-on-Vertex: contributes to stats, no reconstruction.
	deferredIn := `{"anthropic_version":"vertex-2023-10-16","messages":[{"role":"user","content":"hello"}]}`
	deferredOut := `{"content":[{"type":"text","text":"hi"}]}`

	return []model.Record{
		{
			Provider: "bedrock", Timestamp: base, RequestID: "req-bedrock-1",
			ModelID: "claude-opus-4-6", Operation: "Converse", Status: "200",
			Identity: model.Identity{Principal: "arn:aws:iam::111:user/dev", Extra: map[string]string{"region": "eu-west-1"}},
			Input:    model.Body{JSON: json.RawMessage(bedrockIn), ContentType: "application/json", TokenCount: 40, CacheRead: 10, CacheWrite: 4},
			Output:   model.Body{JSON: json.RawMessage(bedrockOut), ContentType: "application/json", TokenCount: 12},
		},
		{
			Provider: "azure", Timestamp: base.Add(time.Minute), RequestID: "req-azure-1",
			ModelID: "gpt-4o", Operation: "ChatCompletions_Create", Status: "200",
			Identity:  model.Identity{Principal: "oid-abc", Extra: map[string]string{"resource": "my-aoai"}},
			Input:     model.Body{JSON: json.RawMessage(azureIn), ContentType: "application/json", TokenCount: 20, CacheRead: 0},
			Output:    model.Body{JSON: json.RawMessage(azureOut), ContentType: "application/json", TokenCount: 1},
			LatencyMs: 800,
		},
		{
			Provider: "vertex", Timestamp: base.Add(2 * time.Minute), RequestID: "req-gemini-1",
			ModelID: "gemini-2.0-flash", Operation: "generateContent", Status: "200",
			Identity:  model.Identity{Principal: "sa@project.iam.gserviceaccount.com"},
			Input:     model.Body{JSON: json.RawMessage(geminiIn), ContentType: "application/json", TokenCount: 15},
			Output:    model.Body{JSON: json.RawMessage(geminiOut), ContentType: "application/json", TokenCount: 3},
			LatencyMs: 600,
		},
		{
			Provider: "vertex", Timestamp: base.Add(3 * time.Minute), RequestID: "req-deferred-1",
			ModelID: "claude-opus-4-6", Operation: "rawPredict", Status: "200",
			Identity: model.Identity{Principal: "sa@project.iam.gserviceaccount.com"},
			Input:    model.Body{JSON: json.RawMessage(deferredIn), ContentType: "application/json", TokenCount: 5},
			Output:   model.Body{JSON: json.RawMessage(deferredOut), ContentType: "application/json", TokenCount: 2},
		},
	}
}

// --- helpers ---

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}

// collectFiles reads every regular file under root, keyed by its slash-separated
// path relative to root.
func collectFiles(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := make(map[string][]byte)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = b
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return files
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
}

// TestStreamingExporterMatchesExport locks the equivalence of the two write
// paths: streaming conversations through an Exporter one at a time must
// produce a byte-identical directory to the batch Export wrapper.
func TestStreamingExporterMatchesExport(t *testing.T) {
	rpt := goldenReport()
	batch, stream := t.TempDir(), t.TempDir()

	if err := ir.Export(rpt, batch, fixedSource, ir.ExportOptions{}); err != nil {
		t.Fatalf("batch export: %v", err)
	}

	e, err := ir.NewExporter(stream, fixedSource, rpt.GeneratedAt, ir.ExportOptions{})
	if err != nil {
		t.Fatalf("NewExporter: %v", err)
	}
	for _, day := range rpt.Days {
		for _, cs := range day.Conversations {
			if err := e.WriteConversation(cs); err != nil {
				t.Fatalf("WriteConversation: %v", err)
			}
		}
	}
	if err := e.Finish(rpt); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	got, want := collectFiles(t, stream), collectFiles(t, batch)
	if len(got) != len(want) {
		t.Fatalf("file set differs: streaming %v, batch %v", keys(got), keys(want))
	}
	for rel, data := range want {
		if string(got[rel]) != string(data) {
			t.Errorf("file %s differs between streaming and batch export", rel)
		}
	}
}

// TestExportEmbedsRawBodiesWhenAsked covers the opt-in path: with RawBodies
// set, invocation payloads are embedded verbatim and the manifest says so.
func TestExportEmbedsRawBodiesWhenAsked(t *testing.T) {
	rpt := goldenReport()
	tmp := t.TempDir()
	if err := ir.Export(rpt, tmp, fixedSource, ir.ExportOptions{RawBodies: true}); err != nil {
		t.Fatalf("export: %v", err)
	}

	manifest, err := os.ReadFile(filepath.Join(tmp, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m struct {
		RawBodies string `json:"raw_bodies"`
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatalf("manifest not JSON: %v", err)
	}
	if m.RawBodies != "embedded" {
		t.Errorf("manifest raw_bodies = %q, want embedded", m.RawBodies)
	}

	conv, err := os.ReadFile(filepath.Join(tmp, "conversations", "bedrock-sess-1.json"))
	if err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	var c struct {
		Invocations []struct {
			Input struct {
				JSON json.RawMessage `json:"json"`
			} `json:"input"`
		} `json:"invocations"`
	}
	if err := json.Unmarshal(conv, &c); err != nil {
		t.Fatalf("conversation not JSON: %v", err)
	}
	if len(c.Invocations) == 0 || len(c.Invocations[0].Input.JSON) == 0 {
		t.Error("expected embedded input payload with RawBodies: true")
	}

	// And the default path omits them.
	tmp2 := t.TempDir()
	if err := ir.Export(rpt, tmp2, fixedSource, ir.ExportOptions{}); err != nil {
		t.Fatalf("export (default): %v", err)
	}
	conv2, err := os.ReadFile(filepath.Join(tmp2, "conversations", "bedrock-sess-1.json"))
	if err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if strings.Contains(string(conv2), `"json":`) {
		t.Error("default export embedded a raw payload; want omitted")
	}
}
