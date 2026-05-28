package report_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rxbynerd/dressage/internal/model"
	"github.com/rxbynerd/dressage/internal/report"
	"github.com/rxbynerd/dressage/internal/summary"
)

// updateGolden regenerates the golden file instead of asserting against it.
// Run: go test ./internal/report/ -run Golden -update
var updateGolden = flag.Bool("update", false, "update golden report fixture")

// fixedGeneratedAt pins the report's GeneratedAt so rendered output is
// deterministic across runs (the template renders this timestamp).
var fixedGeneratedAt = time.Date(2024, 1, 20, 12, 0, 0, 0, time.UTC)

// TestGoldenReport renders a representative set of Bedrock invocation logs and
// compares the HTML byte-for-byte against a committed golden file. It exists to
// lock the report output invariant across the provider-abstraction refactor:
// the same logical logs must render identically before and after.
func TestGoldenReport(t *testing.T) {
	rpt := summary.Summarize(goldenFixtureLogs())
	rpt.GeneratedAt = fixedGeneratedAt
	rpt.Title = "Bedrock Invocation Log Report"

	var buf bytes.Buffer
	if err := report.Render(rpt, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}

	goldenPath := filepath.Join("testdata", "golden_report.html")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(goldenPath, buf.Bytes(), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("wrote golden file %s (%d bytes)", goldenPath, buf.Len())
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if !bytes.Equal(want, buf.Bytes()) {
		t.Errorf("rendered report differs from golden file %s.\n"+
			"If this change is intentional, re-run with -update and review the diff.", goldenPath)
	}
}

// goldenFixtureLogs builds a deterministic set of Bedrock invocation logs that
// exercises the main rendering paths: session-grouped reconstruction with
// streaming output (thinking + text + tool_use + tool_result + metrics + cache),
// a non-streaming reconstructed response with an array-format system prompt,
// time-gap grouping without reconstruction, and an error record.
func goldenFixtureLogs() []model.InvocationLog {
	base := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	const opStream = "InvokeModelWithResponseStream"
	const opConverse = "Converse"
	const opus = "claude-opus-4-6"
	const haiku = "claude-haiku-4-5"
	const devARN = "arn:aws:iam::111:user/dev"
	const ciARN = "arn:aws:iam::111:role/ci"

	// Conversation A, invocation 1: user asks, assistant thinks + replies + tool_use.
	a1In := `{
		"metadata":{"user_id":"user_hash_account__session_sess-aaaa-1111"},
		"system":"You are a helpful coding assistant.",
		"messages":[{"role":"user","content":"Read config.yaml and summarize it."}],
		"tools":[{"name":"Read","description":"Reads a file from disk"},{"name":"Bash","description":"Runs a shell command"}]
	}`
	a1Out := `[
		{"type":"message_start","message":{"id":"msg_a1","role":"assistant","content":[],"usage":{"input_tokens":50,"output_tokens":0}}},
		{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}},
		{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"The user wants me to read a file."}},
		{"type":"content_block_stop","index":0},
		{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}},
		{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"I'll read it now."}},
		{"type":"content_block_stop","index":1},
		{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_a1","name":"Read","input":""}},
		{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"file_path\":\"config.yaml\"}"}},
		{"type":"content_block_stop","index":2},
		{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":30}},
		{"type":"message_stop","amazon-bedrock-invocationMetrics":{"inputTokenCount":50,"outputTokenCount":30,"invocationLatency":2200,"firstByteLatency":180}}
	]`

	// Conversation A, invocation 2: tool_result fed back, assistant final reply (with cache).
	a2In := `{
		"metadata":{"user_id":"user_hash_account__session_sess-aaaa-1111"},
		"system":"You are a helpful coding assistant.",
		"messages":[
			{"role":"user","content":"Read config.yaml and summarize it."},
			{"role":"assistant","content":[{"type":"thinking","thinking":"The user wants me to read a file."},{"type":"text","text":"I'll read it now."},{"type":"tool_use","id":"toolu_a1","name":"Read","input":{"file_path":"config.yaml"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_a1","content":"port: 8080\nhost: localhost"}]}
		],
		"tools":[{"name":"Read","description":"Reads a file from disk"},{"name":"Bash","description":"Runs a shell command"}]
	}`
	a2Out := `[
		{"type":"message_start","message":{"id":"msg_a2","role":"assistant","content":[],"usage":{"input_tokens":120,"output_tokens":0,"cache_read_input_tokens":40,"cache_creation_input_tokens":10}}},
		{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}},
		{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"The config sets port 8080 on localhost."}},
		{"type":"content_block_stop","index":0},
		{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":18}},
		{"type":"message_stop","amazon-bedrock-invocationMetrics":{"inputTokenCount":120,"outputTokenCount":18,"invocationLatency":1600,"firstByteLatency":120,"cacheReadInputTokenCount":40,"cacheWriteInputTokenCount":10}}
	]`

	// Conversation A2: separate session, non-streaming output, array-format system prompt.
	bIn := `{
		"metadata":{"user_id":"user_hash_account__session_sess-bbbb-2222"},
		"system":[{"type":"text","text":"System part one."},{"type":"text","text":"System part two."}],
		"messages":[{"role":"user","content":"What is 2+2?"}],
		"tools":[]
	}`
	bOut := `{"id":"msg_b1","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"2 + 2 = 4."}]}`

	// Conversation C: error record, no session (time-gap grouping, no reconstruction).
	cIn := `{"messages":[{"role":"user","content":"hello"}]}`

	return []model.InvocationLog{
		{
			Timestamp: base, RequestID: "req-a1", ModelID: opus, Operation: opStream, Status: "200",
			Identity: model.Identity{ARN: devARN},
			Input:    model.InvocationInput{InputBodyJSON: json.RawMessage(a1In), InputContentType: "application/json", InputTokenCount: 50},
			Output:   model.InvocationOutput{OutputBodyJSON: json.RawMessage(a1Out), OutputContentType: "application/json", OutputTokenCount: 30},
		},
		{
			Timestamp: base.Add(8 * time.Second), RequestID: "req-a2", ModelID: opus, Operation: opStream, Status: "200",
			Identity: model.Identity{ARN: devARN},
			Input:    model.InvocationInput{InputBodyJSON: json.RawMessage(a2In), InputContentType: "application/json", InputTokenCount: 120, CacheReadInputTokenCount: 40, CacheWriteInputTokenCount: 10},
			Output:   model.InvocationOutput{OutputBodyJSON: json.RawMessage(a2Out), OutputContentType: "application/json", OutputTokenCount: 18},
		},
		{
			Timestamp: base.Add(20 * time.Minute), RequestID: "req-b1", ModelID: opus, Operation: opConverse, Status: "200",
			Identity: model.Identity{ARN: devARN},
			Input:    model.InvocationInput{InputBodyJSON: json.RawMessage(bIn), InputContentType: "application/json", InputTokenCount: 12},
			Output:   model.InvocationOutput{OutputBodyJSON: json.RawMessage(bOut), OutputContentType: "application/json", OutputTokenCount: 8},
		},
		{
			Timestamp: base.Add(40 * time.Minute), RequestID: "req-c1", ModelID: haiku, Operation: opStream, Status: "429", ErrorCode: "ThrottlingException",
			Identity: model.Identity{ARN: ciARN},
			Input:    model.InvocationInput{InputBodyJSON: json.RawMessage(cIn), InputContentType: "application/json", InputTokenCount: 5},
			Output:   model.InvocationOutput{OutputTokenCount: 0},
		},
	}
}
