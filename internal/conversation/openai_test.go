package conversation

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rxbynerd/dressage/internal/model"
)

// makeAzureRecord builds an azure-provider record from request/response JSON.
func makeAzureRecord(ts time.Time, reqID, input, output string) model.Record {
	return model.Record{
		Provider:  "azure",
		Timestamp: ts,
		RequestID: reqID,
		ModelID:   "gpt-4o",
		Operation: "ChatCompletions_Create",
		Status:    "200",
		Identity:  model.Identity{Principal: "oid-123"},
		Input:     model.Body{JSON: json.RawMessage(input)},
		Output:    model.Body{JSON: json.RawMessage(output)},
	}
}

func TestReconstructOpenAIPlainText(t *testing.T) {
	base := time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC)

	input := `{
		"messages": [
			{"role": "system", "content": "You are concise."},
			{"role": "user", "content": "What is 2+2?"}
		]
	}`
	output := `{
		"choices": [
			{"message": {"role": "assistant", "content": "4"}, "finish_reason": "stop"}
		],
		"usage": {"prompt_tokens": 12, "completion_tokens": 1}
	}`

	detail := Reconstruct([]model.Record{makeAzureRecord(base, "req-1", input, output)})
	if detail == nil {
		t.Fatal("expected non-nil detail")
	}
	if detail.SystemPrompt != "You are concise." {
		t.Errorf("SystemPrompt = %q, want 'You are concise.'", detail.SystemPrompt)
	}
	// Turns: [0] user "What is 2+2?", [1] assistant "4". System is not a turn.
	if len(detail.Turns) != 2 {
		t.Fatalf("turns = %d, want 2", len(detail.Turns))
	}
	if detail.Turns[0].Role != "user" || detail.Turns[0].Blocks[0].Text != "What is 2+2?" {
		t.Errorf("turn[0] = %+v, want user 'What is 2+2?'", detail.Turns[0])
	}
	if detail.Turns[1].Role != "assistant" || detail.Turns[1].Blocks[0].Text != "4" {
		t.Errorf("turn[1] = %+v, want assistant '4'", detail.Turns[1])
	}
}

func TestReconstructOpenAIToolCalls(t *testing.T) {
	base := time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC)

	// Final invocation carries the full history: user → assistant tool_calls →
	// tool result → assistant final answer (from the response body).
	input := `{
		"messages": [
			{"role": "system", "content": "You can use tools."},
			{"role": "user", "content": "What is the weather in Paris?"},
			{"role": "assistant", "content": null, "tool_calls": [
				{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"Paris\"}"}}
			]},
			{"role": "tool", "tool_call_id": "call_1", "content": "18C and sunny"}
		],
		"tools": [{"type": "function", "function": {"name": "get_weather", "description": "Get weather"}}]
	}`
	output := `{
		"choices": [
			{"message": {"role": "assistant", "content": "It is 18C and sunny in Paris."}, "finish_reason": "stop"}
		],
		"usage": {"prompt_tokens": 40, "completion_tokens": 9}
	}`

	detail := Reconstruct([]model.Record{makeAzureRecord(base, "req-1", input, output)})
	if detail == nil {
		t.Fatal("expected non-nil detail")
	}

	// Turns: [0] user, [1] assistant tool_use, [2] user tool_result, [3] assistant final.
	if len(detail.Turns) != 4 {
		t.Fatalf("turns = %d, want 4", len(detail.Turns))
	}

	if detail.Turns[0].Role != "user" {
		t.Errorf("turn[0].Role = %q, want user", detail.Turns[0].Role)
	}

	// Assistant tool_use turn.
	assistant := detail.Turns[1]
	if assistant.Role != "assistant" {
		t.Errorf("turn[1].Role = %q, want assistant", assistant.Role)
	}
	if len(assistant.Blocks) != 1 || assistant.Blocks[0].Type != "tool_use" {
		t.Fatalf("turn[1].Blocks = %+v, want one tool_use", assistant.Blocks)
	}
	tu := assistant.Blocks[0]
	if tu.ToolName != "get_weather" {
		t.Errorf("tool_use ToolName = %q, want get_weather", tu.ToolName)
	}
	if tu.ToolID != "call_1" {
		t.Errorf("tool_use ToolID = %q, want call_1", tu.ToolID)
	}
	// Arguments string is pretty-printed JSON.
	if tu.ToolInput == "" || tu.ToolInput == `{"city":"Paris"}` {
		t.Errorf("tool_use ToolInput = %q, want pretty-printed JSON", tu.ToolInput)
	}

	// Tool result rendered as a user turn.
	toolTurn := detail.Turns[2]
	if toolTurn.Role != "user" {
		t.Errorf("turn[2].Role = %q, want user (tool result)", toolTurn.Role)
	}
	if len(toolTurn.Blocks) != 1 || toolTurn.Blocks[0].Type != "tool_result" {
		t.Fatalf("turn[2].Blocks = %+v, want one tool_result", toolTurn.Blocks)
	}
	tr := toolTurn.Blocks[0]
	if tr.ToolID != "call_1" {
		t.Errorf("tool_result ToolID = %q, want call_1", tr.ToolID)
	}
	if tr.ResultContent != "18C and sunny" {
		t.Errorf("tool_result ResultContent = %q, want '18C and sunny'", tr.ResultContent)
	}

	// Final assistant answer from response body.
	if detail.Turns[3].Role != "assistant" {
		t.Errorf("turn[3].Role = %q, want assistant", detail.Turns[3].Role)
	}
	if detail.Turns[3].Blocks[0].Text != "It is 18C and sunny in Paris." {
		t.Errorf("turn[3] text = %q", detail.Turns[3].Blocks[0].Text)
	}
}

func TestReconstructOpenAILegacyFunctionCall(t *testing.T) {
	base := time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC)

	input := `{
		"messages": [
			{"role": "user", "content": "Weather?"},
			{"role": "assistant", "content": null, "function_call": {"name": "get_weather", "arguments": "{\"city\":\"Paris\"}"}}
		]
	}`
	output := `{"choices": [{"message": {"role": "assistant", "content": "done"}, "finish_reason": "stop"}]}`

	detail := Reconstruct([]model.Record{makeAzureRecord(base, "req-1", input, output)})
	if detail == nil {
		t.Fatal("expected non-nil detail")
	}

	// turn[1] is the assistant message carrying the legacy function_call.
	assistant := detail.Turns[1]
	if len(assistant.Blocks) != 1 || assistant.Blocks[0].Type != "tool_use" {
		t.Fatalf("turn[1].Blocks = %+v, want one tool_use", assistant.Blocks)
	}
	if assistant.Blocks[0].ToolName != "get_weather" {
		t.Errorf("function_call ToolName = %q, want get_weather", assistant.Blocks[0].ToolName)
	}
	if assistant.Blocks[0].ToolID != "" {
		t.Errorf("legacy function_call ToolID = %q, want empty", assistant.Blocks[0].ToolID)
	}
}

func TestReconstructOpenAIStreamingResponse(t *testing.T) {
	base := time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC)

	input := `{"messages": [{"role": "user", "content": "Say hi and call a tool."}]}`
	// Streaming-summarized: an array of chat.completion.chunk objects.
	output := `[
		{"choices": [{"delta": {"role": "assistant", "content": "Hi"}}]},
		{"choices": [{"delta": {"content": " there"}}]},
		{"choices": [{"delta": {"tool_calls": [{"index": 0, "id": "call_x", "function": {"name": "do_thing", "arguments": "{\"a\":"}}]}}]},
		{"choices": [{"delta": {"tool_calls": [{"index": 0, "function": {"arguments": "1}"}}]}}]},
		{"choices": [{"delta": {}, "finish_reason": "tool_calls"}]}
	]`

	detail := Reconstruct([]model.Record{makeAzureRecord(base, "req-1", input, output)})
	if detail == nil {
		t.Fatal("expected non-nil detail")
	}

	// Turns: [0] user, [1] aggregated assistant from streamed chunks.
	if len(detail.Turns) != 2 {
		t.Fatalf("turns = %d, want 2", len(detail.Turns))
	}
	final := detail.Turns[1]
	if final.Role != "assistant" {
		t.Errorf("final turn role = %q, want assistant", final.Role)
	}
	if len(final.Blocks) != 2 {
		t.Fatalf("final blocks = %d, want 2 (text + tool_use)", len(final.Blocks))
	}
	if final.Blocks[0].Type != "text" || final.Blocks[0].Text != "Hi there" {
		t.Errorf("final block[0] = %+v, want text 'Hi there'", final.Blocks[0])
	}
	tu := final.Blocks[1]
	if tu.Type != "tool_use" || tu.ToolName != "do_thing" || tu.ToolID != "call_x" {
		t.Errorf("final block[1] = %+v, want tool_use do_thing/call_x", tu)
	}
	// Aggregated arguments "{\"a\":" + "1}" → {"a":1}, pretty-printed.
	if tu.ToolInput == "" {
		t.Error("aggregated tool_use ToolInput is empty, want pretty JSON")
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(tu.ToolInput), &parsed); err != nil {
		t.Errorf("aggregated ToolInput is not valid JSON: %v (%q)", err, tu.ToolInput)
	} else if parsed["a"] != float64(1) {
		t.Errorf("aggregated args = %v, want a=1", parsed)
	}
}

func TestReconstructOpenAISystemPromptAndTools(t *testing.T) {
	base := time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC)

	longDesc := ""
	for i := 0; i < 250; i++ {
		longDesc += "x"
	}
	input := `{
		"messages": [
			{"role": "system", "content": "First system block."},
			{"role": "developer", "content": [{"type": "text", "text": "Second developer block."}]},
			{"role": "user", "content": "Hello"}
		],
		"tools": [
			{"type": "function", "function": {"name": "short_tool", "description": "short"}},
			{"type": "function", "function": {"name": "long_tool", "description": "` + longDesc + `"}}
		]
	}`
	output := `{"choices": [{"message": {"role": "assistant", "content": "Hi"}, "finish_reason": "stop"}]}`

	detail := Reconstruct([]model.Record{makeAzureRecord(base, "req-1", input, output)})
	if detail == nil {
		t.Fatal("expected non-nil detail")
	}

	// System and developer blocks both fold into SystemPrompt, joined by blank line.
	if detail.SystemPrompt != "First system block.\n\nSecond developer block." {
		t.Errorf("SystemPrompt = %q", detail.SystemPrompt)
	}

	// Neither system nor developer becomes a turn.
	for i, turn := range detail.Turns {
		if turn.Role == "system" || turn.Role == "developer" {
			t.Errorf("turn[%d] has role %q; system/developer must not be turns", i, turn.Role)
		}
	}

	// Tools mapped; long description truncated to 200 chars + "...".
	if len(detail.Tools) != 2 {
		t.Fatalf("tools = %d, want 2", len(detail.Tools))
	}
	if detail.Tools[0].Name != "short_tool" || detail.Tools[0].Description != "short" {
		t.Errorf("tool[0] = %+v", detail.Tools[0])
	}
	if detail.Tools[1].Name != "long_tool" {
		t.Errorf("tool[1].Name = %q, want long_tool", detail.Tools[1].Name)
	}
	if len(detail.Tools[1].Description) != 203 { // 200 chars + "..."
		t.Errorf("tool[1].Description length = %d, want 203 (200 + ellipsis)", len(detail.Tools[1].Description))
	}
}

func TestReconstructOpenAIFinalTurnMetrics(t *testing.T) {
	base := time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC)

	input := `{"messages": [{"role": "user", "content": "Hello"}]}`
	output := `{
		"choices": [{"message": {"role": "assistant", "content": "Hi!"}, "finish_reason": "stop"}],
		"usage": {"prompt_tokens": 100, "completion_tokens": 5, "prompt_tokens_details": {"cached_tokens": 80}}
	}`

	rec := makeAzureRecord(base, "req-99", input, output)
	rec.LatencyMs = 1234
	detail := Reconstruct([]model.Record{rec})
	if detail == nil {
		t.Fatal("expected non-nil detail")
	}

	final := detail.Turns[len(detail.Turns)-1]
	if final.Metrics == nil {
		t.Fatal("final turn should have metrics")
	}
	m := final.Metrics
	if m.RequestID != "req-99" {
		t.Errorf("RequestID = %q, want req-99", m.RequestID)
	}
	if m.ModelID != "gpt-4o" {
		t.Errorf("ModelID = %q, want gpt-4o", m.ModelID)
	}
	if m.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100", m.InputTokens)
	}
	if m.OutputTokens != 5 {
		t.Errorf("OutputTokens = %d, want 5", m.OutputTokens)
	}
	if m.CacheReadTokens != 80 {
		t.Errorf("CacheReadTokens = %d, want 80", m.CacheReadTokens)
	}
	if m.CacheWriteTokens != 0 {
		t.Errorf("CacheWriteTokens = %d, want 0 (OpenAI has no cache-write)", m.CacheWriteTokens)
	}
	if m.LatencyMs != 1234 {
		t.Errorf("LatencyMs = %d, want 1234", m.LatencyMs)
	}
	if m.StopReason != "stop" {
		t.Errorf("StopReason = %q, want stop", m.StopReason)
	}
}

func TestExtractSessionOpenAI(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "top-level user with session",
			input: `{"user": "user_abc_account__session_11112222-3333-4444-5555-666677778888", "messages": []}`,
			want:  "11112222-3333-4444-5555-666677778888",
		},
		{
			name:  "top-level user without session marker",
			input: `{"user": "just-a-plain-user-id", "messages": []}`,
			want:  "",
		},
		{
			name:  "fallback to metadata.user_id",
			input: `{"metadata": {"user_id": "user_x_account__session_aaaa"}, "messages": []}`,
			want:  "aaaa",
		},
		{
			name:  "no user at all",
			input: `{"messages": []}`,
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
			got := ExtractSessionID("azure", "gpt-4o", json.RawMessage(tc.input))
			if got != tc.want {
				t.Errorf("ExtractSessionID(azure) = %q, want %q", got, tc.want)
			}
		})
	}
}

// An assistant message with content:null and no tool_calls carries no blocks
// and must NOT produce a blank turn in the reconstruction.
func TestReconstructOpenAISkipsEmptyAssistantTurn(t *testing.T) {
	base := time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC)

	input := `{
		"messages": [
			{"role": "user", "content": "Hello"},
			{"role": "assistant", "content": null}
		]
	}`
	output := `{"choices": [{"message": {"role": "assistant", "content": "Hi!"}, "finish_reason": "stop"}]}`

	detail := Reconstruct([]model.Record{makeAzureRecord(base, "req-1", input, output)})
	if detail == nil {
		t.Fatal("expected non-nil detail")
	}

	// Expected turns: [0] user "Hello", [1] assistant "Hi!" from the response.
	// The empty assistant history message must be skipped.
	if len(detail.Turns) != 2 {
		t.Fatalf("turns = %d, want 2 (empty assistant turn skipped)", len(detail.Turns))
	}
	for i, turn := range detail.Turns {
		if len(turn.Blocks) == 0 {
			t.Errorf("turn[%d] has zero blocks; empty turns must be skipped", i)
		}
	}
	if detail.Turns[0].Role != "user" || detail.Turns[0].Blocks[0].Text != "Hello" {
		t.Errorf("turn[0] = %+v, want user 'Hello'", detail.Turns[0])
	}
	if detail.Turns[1].Role != "assistant" || detail.Turns[1].Blocks[0].Text != "Hi!" {
		t.Errorf("turn[1] = %+v, want assistant 'Hi!'", detail.Turns[1])
	}
}
