package conversation

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rxbynerd/dressage/internal/model"
)

// makeVertexRecord builds a vertex-provider Gemini record from request/response JSON.
func makeVertexRecord(ts time.Time, reqID, input, output string) model.Record {
	return model.Record{
		Provider:  "vertex",
		Timestamp: ts,
		RequestID: reqID,
		ModelID:   "gemini-2.0-flash",
		Operation: "generateContent",
		Input:     model.Body{JSON: json.RawMessage(input)},
		Output:    model.Body{JSON: json.RawMessage(output)},
	}
}

func TestReconstructGeminiPlainText(t *testing.T) {
	base := time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC)

	input := `{
		"systemInstruction": {"parts": [{"text": "You are concise."}]},
		"contents": [
			{"role": "user", "parts": [{"text": "What is 2+2?"}]}
		]
	}`
	output := `{
		"candidates": [
			{"content": {"role": "model", "parts": [{"text": "4"}]}, "finishReason": "STOP"}
		],
		"usageMetadata": {"promptTokenCount": 12, "candidatesTokenCount": 1, "cachedContentTokenCount": 4}
	}`

	detail := Reconstruct([]model.Record{makeVertexRecord(base, "req-1", input, output)})
	if detail == nil {
		t.Fatal("expected non-nil detail")
	}
	if detail.SystemPrompt != "You are concise." {
		t.Errorf("SystemPrompt = %q", detail.SystemPrompt)
	}
	if len(detail.Turns) != 2 {
		t.Fatalf("turns = %d, want 2", len(detail.Turns))
	}
	if detail.Turns[0].Role != "user" || detail.Turns[0].Blocks[0].Text != "What is 2+2?" {
		t.Errorf("turn[0] = %+v, want user 'What is 2+2?'", detail.Turns[0])
	}
	if detail.Turns[1].Role != "assistant" || detail.Turns[1].Blocks[0].Text != "4" {
		t.Errorf("turn[1] = %+v, want assistant '4'", detail.Turns[1])
	}
	m := detail.Turns[1].Metrics
	if m == nil {
		t.Fatal("assistant turn missing metrics")
	}
	if m.InputTokens != 12 || m.OutputTokens != 1 || m.CacheReadTokens != 4 {
		t.Errorf("metrics tokens = %d/%d/%d, want 12/1/4", m.InputTokens, m.OutputTokens, m.CacheReadTokens)
	}
	if m.CacheWriteTokens != 0 {
		t.Errorf("CacheWriteTokens = %d, want 0", m.CacheWriteTokens)
	}
	if m.StopReason != "STOP" {
		t.Errorf("StopReason = %q, want STOP", m.StopReason)
	}
}

func TestReconstructGeminiFunctionCall(t *testing.T) {
	base := time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC)

	// Single-turn function call: the user asks, the model responds with a
	// functionCall in the output body.
	input := `{
		"contents": [
			{"role": "user", "parts": [{"text": "Weather in Paris?"}]}
		],
		"tools": [{"functionDeclarations": [{"name": "get_weather", "description": "Get the weather"}]}]
	}`
	output := `{
		"candidates": [
			{"content": {"role": "model", "parts": [
				{"functionCall": {"name": "get_weather", "args": {"city": "Paris"}}}
			]}, "finishReason": "STOP"}
		],
		"usageMetadata": {"promptTokenCount": 20, "candidatesTokenCount": 5}
	}`

	detail := Reconstruct([]model.Record{makeVertexRecord(base, "req-1", input, output)})
	if detail == nil {
		t.Fatal("expected non-nil detail")
	}
	if len(detail.Tools) != 1 || detail.Tools[0].Name != "get_weather" {
		t.Errorf("Tools = %+v, want [get_weather]", detail.Tools)
	}
	// Turns: [0] user, [1] assistant functionCall (from output).
	if len(detail.Turns) != 2 {
		t.Fatalf("turns = %d, want 2", len(detail.Turns))
	}
	tu := detail.Turns[1].Blocks[0]
	if tu.Type != "tool_use" || tu.ToolName != "get_weather" {
		t.Errorf("block = %+v, want tool_use get_weather", tu)
	}
	if tu.ToolInput == "" || tu.ToolInput == "{}" {
		t.Errorf("ToolInput = %q, want pretty-printed args", tu.ToolInput)
	}
}

func TestReconstructGeminiMultiTurnToolLoop(t *testing.T) {
	base := time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC)

	// Final invocation carries the full history: user → model functionCall →
	// user functionResponse → model final answer (from the output body).
	input := `{
		"contents": [
			{"role": "user", "parts": [{"text": "Weather in Paris?"}]},
			{"role": "model", "parts": [{"functionCall": {"name": "get_weather", "args": {"city": "Paris"}}}]},
			{"role": "user", "parts": [{"functionResponse": {"name": "get_weather", "response": {"tempC": 18, "sky": "sunny"}}}]}
		],
		"tools": [{"functionDeclarations": [{"name": "get_weather", "description": "Get the weather"}]}]
	}`
	output := `{
		"candidates": [
			{"content": {"role": "model", "parts": [{"text": "It is 18C and sunny in Paris."}]}, "finishReason": "STOP"}
		],
		"usageMetadata": {"promptTokenCount": 40, "candidatesTokenCount": 9}
	}`

	detail := Reconstruct([]model.Record{makeVertexRecord(base, "req-1", input, output)})
	if detail == nil {
		t.Fatal("expected non-nil detail")
	}
	// Turns: [0] user text, [1] assistant tool_use, [2] user tool_result, [3] assistant final.
	if len(detail.Turns) != 4 {
		t.Fatalf("turns = %d, want 4: %+v", len(detail.Turns), detail.Turns)
	}
	if detail.Turns[1].Role != "assistant" || detail.Turns[1].Blocks[0].Type != "tool_use" {
		t.Errorf("turn[1] = %+v, want assistant tool_use", detail.Turns[1])
	}
	tr := detail.Turns[2]
	if tr.Role != "user" || tr.Blocks[0].Type != "tool_result" {
		t.Errorf("turn[2] = %+v, want user tool_result", tr)
	}
	if tr.Blocks[0].ToolID != "get_weather" {
		t.Errorf("tool_result ToolID = %q, want get_weather (function name)", tr.Blocks[0].ToolID)
	}
	if detail.Turns[3].Role != "assistant" || detail.Turns[3].Blocks[0].Text != "It is 18C and sunny in Paris." {
		t.Errorf("turn[3] = %+v, want assistant final answer", detail.Turns[3])
	}
}

func TestReconstructGeminiThinking(t *testing.T) {
	base := time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC)

	input := `{
		"contents": [
			{"role": "user", "parts": [{"text": "Solve it."}]}
		]
	}`
	// A thinking part is a text part flagged thought:true, followed by the answer.
	output := `{
		"candidates": [
			{"content": {"role": "model", "parts": [
				{"text": "Let me reason about this carefully.", "thought": true},
				{"text": "The answer is 42."}
			]}, "finishReason": "STOP"}
		],
		"usageMetadata": {"promptTokenCount": 5, "candidatesTokenCount": 20}
	}`

	detail := Reconstruct([]model.Record{makeVertexRecord(base, "req-1", input, output)})
	if detail == nil {
		t.Fatal("expected non-nil detail")
	}
	final := detail.Turns[len(detail.Turns)-1]
	if len(final.Blocks) != 2 {
		t.Fatalf("final blocks = %d, want 2 (thinking + text)", len(final.Blocks))
	}
	if final.Blocks[0].Type != "thinking" || final.Blocks[0].Text != "Let me reason about this carefully." {
		t.Errorf("block[0] = %+v, want thinking", final.Blocks[0])
	}
	if final.Blocks[1].Type != "text" || final.Blocks[1].Text != "The answer is 42." {
		t.Errorf("block[1] = %+v, want text answer", final.Blocks[1])
	}
}

func TestReconstructGeminiStreamingAggregation(t *testing.T) {
	base := time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC)

	input := `{"contents": [{"role": "user", "parts": [{"text": "Hi"}]}]}`
	// vertexfetch wraps streamed chunks into a JSON array; the reconstructor must
	// concatenate the streamed text and read usage/finishReason from the last chunk.
	output := `[
		{"candidates": [{"content": {"role": "model", "parts": [{"text": "Hel"}]}}]},
		{"candidates": [{"content": {"role": "model", "parts": [{"text": "lo "}]}}]},
		{"candidates": [{"content": {"role": "model", "parts": [{"text": "there!"}]}, "finishReason": "STOP"}], "usageMetadata": {"promptTokenCount": 3, "candidatesTokenCount": 4}}
	]`

	detail := Reconstruct([]model.Record{makeVertexRecord(base, "req-1", input, output)})
	if detail == nil {
		t.Fatal("expected non-nil detail")
	}
	final := detail.Turns[len(detail.Turns)-1]
	if len(final.Blocks) != 1 || final.Blocks[0].Text != "Hello there!" {
		t.Errorf("streamed text = %+v, want one block 'Hello there!'", final.Blocks)
	}
	if final.Metrics == nil || final.Metrics.OutputTokens != 4 || final.Metrics.StopReason != "STOP" {
		t.Errorf("metrics = %+v, want OutputTokens=4 StopReason=STOP", final.Metrics)
	}
}

func TestReconstructGeminiInlineDataMediaBlock(t *testing.T) {
	base := time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC)

	input := `{
		"contents": [
			{"role": "user", "parts": [
				{"inlineData": {"mimeType": "image/png", "data": "iVBORw0KGgo="}},
				{"text": "What is in this image?"}
			]}
		]
	}`
	output := `{"candidates": [{"content": {"role": "model", "parts": [{"text": "A cat."}]}}]}`

	detail := Reconstruct([]model.Record{makeVertexRecord(base, "req-1", input, output)})
	if detail == nil {
		t.Fatal("expected non-nil detail")
	}
	blocks := detail.Turns[0].Blocks
	if len(blocks) != 2 {
		t.Fatalf("user blocks = %d, want 2 (media block + text)", len(blocks))
	}
	if blocks[0].Type != "media" {
		t.Fatalf("block[0].Type = %q, want media", blocks[0].Type)
	}
	if blocks[0].MimeType != "image/png" {
		t.Errorf("block[0].MimeType = %q, want image/png", blocks[0].MimeType)
	}
	if !blocks[0].MediaInline {
		t.Errorf("block[0].MediaInline = false, want true for inlineData")
	}
	// "iVBORw0KGgo=" decodes to 8 bytes.
	if blocks[0].MediaBytes != 8 {
		t.Errorf("block[0].MediaBytes = %d, want 8 (decoded length)", blocks[0].MediaBytes)
	}
	if blocks[0].FileURI != "" {
		t.Errorf("block[0].FileURI = %q, want empty for inline media", blocks[0].FileURI)
	}
	// The caption text must NOT be coalesced onto the media block.
	if blocks[1].Type != "text" || blocks[1].Text != "What is in this image?" {
		t.Errorf("block[1] = %+v, want text caption", blocks[1])
	}
}

func TestReconstructGeminiFileDataMediaBlock(t *testing.T) {
	base := time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC)
	input := `{
		"contents": [
			{"role": "user", "parts": [
				{"fileData": {"mimeType": "application/pdf", "fileUri": "gs://bucket/doc.pdf"}},
				{"text": "Summarize this."}
			]}
		]
	}`
	output := `{"candidates": [{"content": {"role": "model", "parts": [{"text": "ok"}]}}]}`

	detail := Reconstruct([]model.Record{makeVertexRecord(base, "req-1", input, output)})
	if detail == nil {
		t.Fatal("expected non-nil detail")
	}
	blocks := detail.Turns[0].Blocks
	if len(blocks) != 2 {
		t.Fatalf("user blocks = %d, want 2 (media block + text)", len(blocks))
	}
	if blocks[0].Type != "media" {
		t.Fatalf("block[0].Type = %q, want media", blocks[0].Type)
	}
	if blocks[0].MimeType != "application/pdf" {
		t.Errorf("block[0].MimeType = %q, want application/pdf", blocks[0].MimeType)
	}
	if blocks[0].FileURI != "gs://bucket/doc.pdf" {
		t.Errorf("block[0].FileURI = %q, want gs://bucket/doc.pdf", blocks[0].FileURI)
	}
	if blocks[0].MediaInline {
		t.Errorf("block[0].MediaInline = true, want false for fileData reference")
	}
	// The caption text must NOT be coalesced onto the media block.
	if blocks[1].Text != "Summarize this." {
		t.Errorf("caption block = %q, want 'Summarize this.'", blocks[1].Text)
	}
}

func TestInlineDataBlockNoMime(t *testing.T) {
	block := inlineDataBlock(&geminiInlineData{})
	if block.Type != "media" {
		t.Errorf("Type = %q, want media", block.Type)
	}
	if block.MimeType != "" {
		t.Errorf("MimeType = %q, want empty", block.MimeType)
	}
	if !block.MediaInline {
		t.Errorf("MediaInline = false, want true")
	}
	if block.MediaBytes != 0 {
		t.Errorf("MediaBytes = %d, want 0 for absent data", block.MediaBytes)
	}
}

func TestParseGeminiRequestGuards(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"nil body", ``},
		{"malformed json", `{not json`},
		{"valid json no contents", `{"systemInstruction":{"parts":[{"text":"x"}]}}`},
		{"empty contents", `{"contents":[]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if req := parseGeminiRequest([]byte(c.body)); req != nil {
				t.Errorf("parseGeminiRequest(%q) = %+v, want nil", c.body, req)
			}
		})
	}
}

func TestReconstructGeminiAllRecordsEmptyReturnsNil(t *testing.T) {
	base := time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC)
	// Every record has an unparseable/contents-less request, so there is no
	// "best" invocation to reconstruct from.
	rec := makeVertexRecord(base, "req-1", `{"systemInstruction":{"parts":[{"text":"x"}]}}`, `{}`)
	if got := reconstructGemini([]model.Record{rec}); got != nil {
		t.Errorf("reconstructGemini = %+v, want nil when no record has contents", got)
	}
}

func TestGeminiToolsKeepsFullDescriptionAndSchema(t *testing.T) {
	long := strings.Repeat("x", 250)
	schema := json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)
	tools := geminiTools([]geminiTool{{FunctionDeclarations: []geminiFunctionDecl{{
		Name:        "t",
		Description: long,
		Parameters:  schema,
	}}}})
	if len(tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(tools))
	}
	// The full (untruncated) description must survive: truncation now happens at
	// render time, not in reconstruction.
	if tools[0].Description != long {
		t.Errorf("description len = %d, want %d (full, untruncated)", len(tools[0].Description), len(long))
	}
	if string(tools[0].InputSchema) != string(schema) {
		t.Errorf("InputSchema = %s, want %s", tools[0].InputSchema, schema)
	}
}

func TestReconstructGeminiMetricsNotOverwritten(t *testing.T) {
	base := time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC)

	// Two invocations form one conversation: the first turn-pair, then a
	// follow-up that carries the full history. The earlier assistant turn's
	// metrics must come from the invocation that produced it (req-1), not be
	// overwritten by the later invocation.
	in1 := `{"contents":[{"role":"user","parts":[{"text":"Q1"}]}]}`
	out1 := `{"candidates":[{"content":{"role":"model","parts":[{"text":"A1"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2}}`

	in2 := `{"contents":[
		{"role":"user","parts":[{"text":"Q1"}]},
		{"role":"model","parts":[{"text":"A1"}]},
		{"role":"user","parts":[{"text":"Q2"}]}
	]}`
	out2 := `{"candidates":[{"content":{"role":"model","parts":[{"text":"A2"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":30,"candidatesTokenCount":4}}`

	detail := Reconstruct([]model.Record{
		makeVertexRecord(base, "req-1", in1, out1),
		makeVertexRecord(base.Add(time.Minute), "req-2", in2, out2),
	})
	if detail == nil {
		t.Fatal("expected non-nil detail")
	}
	// Turns: [0] user Q1, [1] assistant A1 (req-1 metrics), [2] user Q2, [3] assistant A2 (req-2 metrics).
	if len(detail.Turns) != 4 {
		t.Fatalf("turns = %d, want 4: %+v", len(detail.Turns), detail.Turns)
	}
	m1 := detail.Turns[1].Metrics
	if m1 == nil || m1.RequestID != "req-1" || m1.OutputTokens != 2 {
		t.Errorf("turn[1] metrics = %+v, want req-1 with OutputTokens=2", m1)
	}
	m2 := detail.Turns[3].Metrics
	if m2 == nil || m2.RequestID != "req-2" || m2.OutputTokens != 4 {
		t.Errorf("turn[3] metrics = %+v, want req-2 with OutputTokens=4", m2)
	}
}

func TestExtractSessionGemini(t *testing.T) {
	withMarker := `{
		"systemInstruction": {"parts": [{"text": "user_abc_account__session_11112222-3333-4444-5555-666677778888"}]},
		"contents": [{"role": "user", "parts": [{"text": "hi"}]}]
	}`
	got := ExtractSessionID("vertex", "gemini-2.0-flash", json.RawMessage(withMarker))
	if got != "11112222-3333-4444-5555-666677778888" {
		t.Errorf("session = %q, want the uuid after _session_", got)
	}

	noMarker := `{"contents": [{"role": "user", "parts": [{"text": "hi"}]}]}`
	if got := ExtractSessionID("vertex", "gemini-2.0-flash", json.RawMessage(noMarker)); got != "" {
		t.Errorf("session = %q, want empty when no marker present", got)
	}

	// Malformed JSON must yield "" without panicking.
	if got := ExtractSessionID("vertex", "gemini-2.0-flash", json.RawMessage(`{not json`)); got != "" {
		t.Errorf("session = %q, want empty for malformed JSON", got)
	}
}
