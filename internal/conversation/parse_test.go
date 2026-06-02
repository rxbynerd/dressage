package conversation

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rxbynerd/dressage/internal/model"
)

func TestExtractSessionID(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "valid session ID (legacy _session_ suffix)",
			input: `{"metadata":{"user_id":"user_abc123_account__session_99bfe2b8-362c-41fc-be2b-a9e707c1e9c7"}}`,
			want:  "99bfe2b8-362c-41fc-be2b-a9e707c1e9c7",
		},
		{
			name:  "json object user_id with session_id (current encoding)",
			input: `{"metadata":{"user_id":"{\"device_id\":\"d1\",\"account_uuid\":\"\",\"session_id\":\"72a86c1d-8b6c-4de2-9583-d294f804d75e\"}"}}`,
			want:  "72a86c1d-8b6c-4de2-9583-d294f804d75e",
		},
		{
			name:  "json object user_id with empty session_id",
			input: `{"metadata":{"user_id":"{\"device_id\":\"d1\",\"session_id\":\"\"}"}}`,
			want:  "",
		},
		{
			name:  "no metadata",
			input: `{"messages":[]}`,
			want:  "",
		},
		{
			name:  "empty user_id",
			input: `{"metadata":{"user_id":""}}`,
			want:  "",
		},
		{
			name:  "no session in user_id",
			input: `{"metadata":{"user_id":"user_abc123_account_"}}`,
			want:  "",
		},
		{
			name:  "empty body",
			input: ``,
			want:  "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractSessionID("bedrock", "anthropic.claude-3", json.RawMessage(tc.input))
			if got != tc.want {
				t.Errorf("ExtractSessionID() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExtractSystemPrompt(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "string format",
			input: `"You are Claude Code."`,
			want:  "You are Claude Code.",
		},
		{
			name:  "array format",
			input: `[{"type":"text","text":"Header"},{"type":"text","text":"Body"}]`,
			want:  "Header\n\nBody",
		},
		{
			name:  "empty",
			input: ``,
			want:  "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractSystemPrompt(json.RawMessage(tc.input))
			if got != tc.want {
				t.Errorf("extractSystemPrompt() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestConvertMessage(t *testing.T) {
	t.Run("string content", func(t *testing.T) {
		msg := apiMessage{
			Role:    "user",
			Content: json.RawMessage(`"Hello, world!"`),
		}
		turn := convertMessage(msg)
		if turn.Role != "user" {
			t.Errorf("role = %q, want %q", turn.Role, "user")
		}
		if len(turn.Blocks) != 1 {
			t.Fatalf("blocks = %d, want 1", len(turn.Blocks))
		}
		if turn.Blocks[0].Type != "text" || turn.Blocks[0].Text != "Hello, world!" {
			t.Errorf("block = %+v, want text 'Hello, world!'", turn.Blocks[0])
		}
	})

	t.Run("array content with tool_use", func(t *testing.T) {
		msg := apiMessage{
			Role: "assistant",
			Content: json.RawMessage(`[
				{"type":"text","text":"Let me check."},
				{"type":"tool_use","id":"toolu_123","name":"Read","input":{"file_path":"/tmp/test"}}
			]`),
		}
		turn := convertMessage(msg)
		if len(turn.Blocks) != 2 {
			t.Fatalf("blocks = %d, want 2", len(turn.Blocks))
		}
		if turn.Blocks[0].Type != "text" {
			t.Errorf("block[0].Type = %q, want text", turn.Blocks[0].Type)
		}
		if turn.Blocks[1].Type != "tool_use" {
			t.Errorf("block[1].Type = %q, want tool_use", turn.Blocks[1].Type)
		}
		if turn.Blocks[1].ToolName != "Read" {
			t.Errorf("block[1].ToolName = %q, want Read", turn.Blocks[1].ToolName)
		}
		if turn.Blocks[1].ToolID != "toolu_123" {
			t.Errorf("block[1].ToolID = %q, want toolu_123", turn.Blocks[1].ToolID)
		}
	})

	t.Run("tool_result with string content", func(t *testing.T) {
		msg := apiMessage{
			Role: "user",
			Content: json.RawMessage(`[
				{"type":"tool_result","tool_use_id":"toolu_123","content":"file contents here"}
			]`),
		}
		turn := convertMessage(msg)
		if len(turn.Blocks) != 1 {
			t.Fatalf("blocks = %d, want 1", len(turn.Blocks))
		}
		if turn.Blocks[0].ResultContent != "file contents here" {
			t.Errorf("ResultContent = %q, want 'file contents here'", turn.Blocks[0].ResultContent)
		}
	})

	t.Run("tool_result with array content", func(t *testing.T) {
		msg := apiMessage{
			Role: "user",
			Content: json.RawMessage(`[
				{"type":"tool_result","tool_use_id":"toolu_456","content":[{"type":"text","text":"line 1"},{"type":"text","text":"line 2"}]}
			]`),
		}
		turn := convertMessage(msg)
		if len(turn.Blocks) != 1 {
			t.Fatalf("blocks = %d, want 1", len(turn.Blocks))
		}
		if turn.Blocks[0].ResultContent != "line 1\nline 2" {
			t.Errorf("ResultContent = %q, want 'line 1\\nline 2'", turn.Blocks[0].ResultContent)
		}
	})

	t.Run("thinking block", func(t *testing.T) {
		msg := apiMessage{
			Role: "assistant",
			Content: json.RawMessage(`[
				{"type":"thinking","thinking":"Let me consider this..."},
				{"type":"text","text":"Here is my answer."}
			]`),
		}
		turn := convertMessage(msg)
		if len(turn.Blocks) != 2 {
			t.Fatalf("blocks = %d, want 2", len(turn.Blocks))
		}
		if turn.Blocks[0].Type != "thinking" || turn.Blocks[0].Text != "Let me consider this..." {
			t.Errorf("block[0] = %+v, want thinking", turn.Blocks[0])
		}
	})
}

func TestReassembleStreamingOutput(t *testing.T) {
	chunks := `[
		{"type":"message_start","message":{"id":"msg_1","role":"assistant","model":"claude-opus-4-6","content":[],"usage":{"input_tokens":100,"output_tokens":0}}},
		{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}},
		{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me "}},
		{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"think..."}},
		{"type":"content_block_stop","index":0},
		{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}},
		{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hello "}},
		{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"world!"}},
		{"type":"content_block_stop","index":1},
		{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"Bash","input":""}},
		{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"command\""}},
		{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":": \"ls\"}"}},
		{"type":"content_block_stop","index":2},
		{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":50}},
		{"type":"message_stop","amazon-bedrock-invocationMetrics":{"inputTokenCount":100,"outputTokenCount":50,"invocationLatency":5000,"firstByteLatency":200,"cacheReadInputTokenCount":80,"cacheWriteInputTokenCount":20}}
	]`

	turn := reassembleOutput(json.RawMessage(chunks))
	if turn == nil {
		t.Fatal("expected non-nil turn")
	}
	if turn.Role != "assistant" {
		t.Errorf("role = %q, want assistant", turn.Role)
	}
	if len(turn.Blocks) != 3 {
		t.Fatalf("blocks = %d, want 3", len(turn.Blocks))
	}

	// Thinking block
	if turn.Blocks[0].Type != "thinking" {
		t.Errorf("block[0].Type = %q, want thinking", turn.Blocks[0].Type)
	}
	if turn.Blocks[0].Text != "Let me think..." {
		t.Errorf("block[0].Text = %q, want 'Let me think...'", turn.Blocks[0].Text)
	}

	// Text block
	if turn.Blocks[1].Type != "text" {
		t.Errorf("block[1].Type = %q, want text", turn.Blocks[1].Type)
	}
	if turn.Blocks[1].Text != "Hello world!" {
		t.Errorf("block[1].Text = %q, want 'Hello world!'", turn.Blocks[1].Text)
	}

	// Tool use block
	if turn.Blocks[2].Type != "tool_use" {
		t.Errorf("block[2].Type = %q, want tool_use", turn.Blocks[2].Type)
	}
	if turn.Blocks[2].ToolName != "Bash" {
		t.Errorf("block[2].ToolName = %q, want Bash", turn.Blocks[2].ToolName)
	}

	// Stop reason
	if turn.Metrics == nil || turn.Metrics.StopReason != "tool_use" {
		t.Errorf("stop_reason = %v, want tool_use", turn.Metrics)
	}
}

func TestExtractStreamMetrics(t *testing.T) {
	chunks := `[
		{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":100}},
		{"type":"message_stop","amazon-bedrock-invocationMetrics":{"inputTokenCount":500,"outputTokenCount":100,"invocationLatency":3000,"firstByteLatency":150,"cacheReadInputTokenCount":400,"cacheWriteInputTokenCount":50}}
	]`

	sm := extractStreamMetrics(json.RawMessage(chunks))
	if sm == nil {
		t.Fatal("expected non-nil metrics")
	}
	if sm.StopReason != "end_turn" {
		t.Errorf("StopReason = %q, want end_turn", sm.StopReason)
	}
	if sm.LatencyMs != 3000 {
		t.Errorf("LatencyMs = %d, want 3000", sm.LatencyMs)
	}
	if sm.FirstByteMs != 150 {
		t.Errorf("FirstByteMs = %d, want 150", sm.FirstByteMs)
	}
	if sm.CacheReadTokens != 400 {
		t.Errorf("CacheReadTokens = %d, want 400", sm.CacheReadTokens)
	}
}

func TestReconstructConversation(t *testing.T) {
	base := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	// Simulate a 2-turn conversation.
	// Invocation 1: user asks question, assistant responds with tool_use.
	inv1Input := `{
		"metadata": {"user_id": "user_hash_account__session_abc-123"},
		"system": "You are helpful.",
		"messages": [
			{"role": "user", "content": "Read my file."}
		],
		"tools": [{"name": "Read", "description": "Reads a file"}]
	}`
	inv1Output := `[
		{"type":"message_start","message":{"id":"msg_1","role":"assistant","content":[]}},
		{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}},
		{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Sure, let me read it."}},
		{"type":"content_block_stop","index":0},
		{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"Read","input":""}},
		{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"file_path\":\"/tmp/test\"}"}},
		{"type":"content_block_stop","index":1},
		{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":30}},
		{"type":"message_stop","amazon-bedrock-invocationMetrics":{"inputTokenCount":50,"outputTokenCount":30,"invocationLatency":2000,"firstByteLatency":100}}
	]`

	// Invocation 2: tool result + assistant final response.
	inv2Input := `{
		"metadata": {"user_id": "user_hash_account__session_abc-123"},
		"system": "You are helpful.",
		"messages": [
			{"role": "user", "content": "Read my file."},
			{"role": "assistant", "content": [
				{"type": "text", "text": "Sure, let me read it."},
				{"type": "tool_use", "id": "toolu_1", "name": "Read", "input": {"file_path": "/tmp/test"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_1", "content": "Hello from the file!"}
			]}
		],
		"tools": [{"name": "Read", "description": "Reads a file"}]
	}`
	inv2Output := `[
		{"type":"message_start","message":{"id":"msg_2","role":"assistant","content":[]}},
		{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}},
		{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"The file says: Hello from the file!"}},
		{"type":"content_block_stop","index":0},
		{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":15}},
		{"type":"message_stop","amazon-bedrock-invocationMetrics":{"inputTokenCount":100,"outputTokenCount":15,"invocationLatency":1500,"firstByteLatency":80}}
	]`

	logs := []model.Record{
		{
			Provider:  "bedrock",
			Timestamp: base,
			RequestID: "req-1",
			ModelID:   "claude-opus-4-6",
			Operation: "InvokeModelWithResponseStream",
			Status:    "200",
			Identity:  model.Identity{Principal: "arn:aws:iam::123:user/alice"},
			Input: model.Body{
				JSON:       json.RawMessage(inv1Input),
				TokenCount: 50,
			},
			Output: model.Body{
				JSON:       json.RawMessage(inv1Output),
				TokenCount: 30,
			},
		},
		{
			Provider:  "bedrock",
			Timestamp: base.Add(5 * time.Second),
			RequestID: "req-2",
			ModelID:   "claude-opus-4-6",
			Operation: "InvokeModelWithResponseStream",
			Status:    "200",
			Identity:  model.Identity{Principal: "arn:aws:iam::123:user/alice"},
			Input: model.Body{
				JSON:       json.RawMessage(inv2Input),
				TokenCount: 100,
			},
			Output: model.Body{
				JSON:       json.RawMessage(inv2Output),
				TokenCount: 15,
			},
		},
	}

	detail := Reconstruct(logs)
	if detail == nil {
		t.Fatal("expected non-nil detail")
	}

	if detail.SessionID != "abc-123" {
		t.Errorf("SessionID = %q, want abc-123", detail.SessionID)
	}
	if detail.SystemPrompt != "You are helpful." {
		t.Errorf("SystemPrompt = %q, want 'You are helpful.'", detail.SystemPrompt)
	}
	if len(detail.Tools) != 1 || detail.Tools[0].Name != "Read" {
		t.Errorf("Tools = %+v, want [Read]", detail.Tools)
	}

	// Expected turns:
	// [0] user: "Read my file."
	// [1] assistant: "Sure, let me read it." + tool_use (from inv2's input messages[1])
	// [2] user: tool_result (from inv2's input messages[2])
	// [3] assistant: "The file says: Hello from the file!" (from inv2's output)
	if len(detail.Turns) != 4 {
		t.Fatalf("turns = %d, want 4", len(detail.Turns))
	}

	if detail.Turns[0].Role != "user" {
		t.Errorf("turn[0].Role = %q, want user", detail.Turns[0].Role)
	}
	if detail.Turns[1].Role != "assistant" {
		t.Errorf("turn[1].Role = %q, want assistant", detail.Turns[1].Role)
	}
	if detail.Turns[2].Role != "user" {
		t.Errorf("turn[2].Role = %q, want user", detail.Turns[2].Role)
	}
	if detail.Turns[3].Role != "assistant" {
		t.Errorf("turn[3].Role = %q, want assistant", detail.Turns[3].Role)
	}

	// Check assistant turn metrics were attached.
	// Turn[1] should have metrics from invocation 1 (which had 1 input message).
	if detail.Turns[1].Metrics == nil {
		t.Error("turn[1] should have metrics")
	} else if detail.Turns[1].Metrics.RequestID != "req-1" {
		t.Errorf("turn[1].Metrics.RequestID = %q, want req-1", detail.Turns[1].Metrics.RequestID)
	}

	// Turn[3] should have metrics from invocation 2.
	if detail.Turns[3].Metrics == nil {
		t.Error("turn[3] should have metrics")
	} else if detail.Turns[3].Metrics.RequestID != "req-2" {
		t.Errorf("turn[3].Metrics.RequestID = %q, want req-2", detail.Turns[3].Metrics.RequestID)
	}

	// Check final turn content.
	if detail.Turns[3].Blocks[0].Text != "The file says: Hello from the file!" {
		t.Errorf("final turn text = %q", detail.Turns[3].Blocks[0].Text)
	}
}

// makeAnthropicRecord builds a bedrock-provider Anthropic record from
// request/response JSON.
func makeAnthropicRecord(ts time.Time, reqID, input, output string) model.Record {
	return model.Record{
		Provider:  "bedrock",
		Timestamp: ts,
		RequestID: reqID,
		ModelID:   "claude-opus-4-6",
		Operation: "InvokeModelWithResponseStream",
		Status:    "200",
		Input:     model.Body{JSON: json.RawMessage(input)},
		Output:    model.Body{JSON: json.RawMessage(output)},
	}
}

func TestExtractToolsKeepsFullDescriptionAndSchema(t *testing.T) {
	longDesc := strings.Repeat("y", 250)
	schema := json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}`)
	input := `{
		"messages": [{"role": "user", "content": "hi"}],
		"tools": [{"name": "Read", "description": "` + longDesc + `", "input_schema": ` + string(schema) + `}]
	}`
	output := `{"id":"msg_1","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"ok"}]}`

	detail := Reconstruct([]model.Record{makeAnthropicRecord(time.Now(), "req-1", input, output)})
	if detail == nil {
		t.Fatal("expected non-nil detail")
	}
	if len(detail.Tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(detail.Tools))
	}
	// The full (untruncated) description must survive.
	if detail.Tools[0].Description != longDesc {
		t.Errorf("description len = %d, want %d (full, untruncated)", len(detail.Tools[0].Description), len(longDesc))
	}
	// The input_schema must be captured verbatim as inline JSON.
	if string(detail.Tools[0].InputSchema) != string(schema) {
		t.Errorf("InputSchema = %s, want %s", detail.Tools[0].InputSchema, schema)
	}
}

func TestConvertContentBlockImageMedia(t *testing.T) {
	// "aGVsbG8=" is base64 for "hello" (5 bytes).
	inlineBlock := convertContentBlock(apiContentBlock{
		Type:   "image",
		Source: &apiImageSource{Type: "base64", MediaType: "image/png", Data: "aGVsbG8="},
	})
	if inlineBlock.Type != "media" {
		t.Fatalf("inline block Type = %q, want media", inlineBlock.Type)
	}
	if inlineBlock.MimeType != "image/png" || !inlineBlock.MediaInline || inlineBlock.MediaBytes != 5 {
		t.Errorf("inline media block = %+v, want image/png inline 5 bytes", inlineBlock)
	}
	if inlineBlock.FileURI != "" {
		t.Errorf("inline media FileURI = %q, want empty", inlineBlock.FileURI)
	}

	urlBlock := convertContentBlock(apiContentBlock{
		Type:   "image",
		Source: &apiImageSource{Type: "url", MediaType: "image/jpeg", URL: "https://example.com/cat.jpg"},
	})
	if urlBlock.Type != "media" {
		t.Fatalf("url block Type = %q, want media", urlBlock.Type)
	}
	if urlBlock.FileURI != "https://example.com/cat.jpg" || urlBlock.MediaInline {
		t.Errorf("url media block = %+v, want external reference", urlBlock)
	}
}

func TestImageSourceBlockUnknownType(t *testing.T) {
	// An unrecognized source type still produces a media block, inferring inline
	// vs external from whichever of URL/Data is populated.
	withData := imageSourceBlock(&apiImageSource{Type: "future-kind", MediaType: "image/webp", Data: "aGVsbG8="})
	if withData.Type != "media" || !withData.MediaInline || withData.MediaBytes != 5 {
		t.Errorf("unknown-type with data = %+v, want inline media of 5 bytes", withData)
	}

	withURL := imageSourceBlock(&apiImageSource{Type: "future-kind", MediaType: "image/webp", URL: "https://example.com/x.webp"})
	if withURL.Type != "media" || withURL.FileURI != "https://example.com/x.webp" || withURL.MediaInline {
		t.Errorf("unknown-type with url = %+v, want external media", withURL)
	}

	// A nil source yields an empty media block rather than panicking.
	if got := imageSourceBlock(nil); got.Type != "media" {
		t.Errorf("nil source block = %+v, want empty media block", got)
	}
}

func TestBase64DecodedLen(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"", 0},
		{"aGVsbG8=", 5},     // "hello", one pad
		{"iVBORw0KGgo=", 8}, // 12 chars, one pad
		{"YWJjZA==", 4},     // "abcd", two pads
		{"YWJj", 3},         // "abc", no pad
		{"YWJjZGVmZ2g=", 8}, // "abcdefgh"-ish, one pad
	}
	for _, c := range cases {
		if got := base64DecodedLen(c.in); got != c.want {
			t.Errorf("base64DecodedLen(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}
