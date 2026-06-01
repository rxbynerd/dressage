package ir

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rxbynerd/dressage/internal/model"
)

func TestStableIDUsesSessionWhenPresent(t *testing.T) {
	cs := model.ConversationSummary{
		ID:        "conv-20250301-0",
		SessionID: "sess-abc-123",
		Provider:  "bedrock",
		ModelID:   "claude-opus-4-6",
		StartTime: time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC),
	}
	if got := stableID(cs); got != "sess-abc-123" {
		t.Errorf("stableID = %q, want the session id verbatim", got)
	}
}

func TestStableIDHashesWhenNoSession(t *testing.T) {
	cs := model.ConversationSummary{
		ID:        "conv-20250301-0",
		Provider:  "bedrock",
		ModelID:   "claude-opus-4-6",
		Identity:  "arn:aws:iam::111:user/dev",
		StartTime: time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC),
	}

	id := stableID(cs)
	if len(id) != 16 {
		t.Fatalf("hashed id length = %d, want 16", len(id))
	}

	// Deterministic: same input yields the same id regardless of the run-order
	// dependent display id.
	cs2 := cs
	cs2.ID = "conv-20250301-99"
	if got := stableID(cs2); got != id {
		t.Errorf("stableID not deterministic: %q != %q", got, id)
	}

	// Sensitive to its inputs: a different start time yields a different id.
	cs3 := cs
	cs3.StartTime = cs.StartTime.Add(time.Second)
	if got := stableID(cs3); got == id {
		t.Errorf("stableID collided for distinct start times: %q", got)
	}
}

func TestMapConversationDeferredProviderHasNullConversation(t *testing.T) {
	cs := model.ConversationSummary{
		ID:           "conv-20250301-0",
		Provider:     "vertex",
		ModelID:      "claude-opus-4-6-on-vertex",
		Identity:     "sa@project.iam.gserviceaccount.com",
		StartTime:    time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC),
		EndTime:      time.Date(2025, 3, 1, 9, 5, 0, 0, time.UTC),
		MessageCount: 2,
		Detail:       nil, // deferred: no reconstruction
		Invocations: []model.Invocation{
			{RequestID: "req-1", ModelID: "claude-opus-4-6-on-vertex", Input: model.Body{JSON: json.RawMessage(`{"a":1}`)}},
			{RequestID: "req-2", ModelID: "claude-opus-4-6-on-vertex", Input: model.Body{JSON: json.RawMessage(`{"a":2}`)}},
		},
	}

	conv := mapConversation(cs)
	if conv.Conversation != nil {
		t.Errorf("deferred provider Conversation = %+v, want nil", conv.Conversation)
	}
	if len(conv.Invocations) != 2 {
		t.Errorf("invocations = %d, want 2 (always populated)", len(conv.Invocations))
	}

	// The conversation field must serialize as JSON null.
	b, err := json.Marshal(conv)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"conversation":null`) {
		t.Errorf("expected \"conversation\":null in output, got: %s", b)
	}

	entry := mapEntry(conv)
	if entry.Reconstructed {
		t.Error("manifest entry Reconstructed = true, want false for deferred provider")
	}
	if entry.TurnCount != 0 {
		t.Errorf("manifest entry TurnCount = %d, want 0", entry.TurnCount)
	}
}

func TestMapToolsKeepsFullSchemaUntruncated(t *testing.T) {
	longDesc := strings.Repeat("z", 250)
	schema := json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`)
	tools := mapTools([]model.ToolDef{{
		Name:        "Bash",
		Description: longDesc,
		InputSchema: schema,
	}})

	if len(tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(tools))
	}
	if tools[0].Description != longDesc {
		t.Errorf("description len = %d, want %d (untruncated)", len(tools[0].Description), len(longDesc))
	}
	if string(tools[0].InputSchema) != string(schema) {
		t.Errorf("InputSchema = %s, want %s", tools[0].InputSchema, schema)
	}
}

func TestMapBlockMedia(t *testing.T) {
	inline := mapBlock(model.ContentBlock{
		Type:        "media",
		MimeType:    "image/png",
		MediaInline: true,
		MediaBytes:  5,
	})
	if inline.Type != "media" || inline.MimeType != "image/png" || !inline.Inline || inline.ByteSize != 5 {
		t.Errorf("inline media block = %+v", inline)
	}
	if inline.FileURI != "" {
		t.Errorf("inline media FileURI = %q, want empty", inline.FileURI)
	}

	external := mapBlock(model.ContentBlock{
		Type:     "media",
		MimeType: "application/pdf",
		FileURI:  "gs://bucket/doc.pdf",
	})
	if external.FileURI != "gs://bucket/doc.pdf" || external.Inline {
		t.Errorf("external media block = %+v", external)
	}
}

func TestMapBlockToolUseInlineJSON(t *testing.T) {
	// A pretty-printed tool input becomes inline JSON.
	block := mapBlock(model.ContentBlock{
		Type:      "tool_use",
		ToolID:    "toolu_1",
		ToolName:  "Bash",
		ToolInput: "{\n  \"command\": \"go test\"\n}",
	})
	if block.ToolInput == nil {
		t.Fatal("ToolInput is nil, want inline JSON")
	}
	var decoded map[string]string
	if err := json.Unmarshal(block.ToolInput, &decoded); err != nil {
		t.Fatalf("ToolInput is not valid JSON object: %v", err)
	}
	if decoded["command"] != "go test" {
		t.Errorf("decoded command = %q, want 'go test'", decoded["command"])
	}
}

func TestToolInputJSONNonJSONPreservedAsString(t *testing.T) {
	raw := toolInputJSON("not json at all")
	if raw == nil {
		t.Fatal("toolInputJSON returned nil for non-JSON input")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("non-JSON input not preserved as a JSON string: %v", err)
	}
	if s != "not json at all" {
		t.Errorf("preserved string = %q, want original", s)
	}
}

func TestMapStatsSumsCacheTokens(t *testing.T) {
	cs := model.ConversationSummary{
		MessageCount: 2,
		InputTokens:  100,
		OutputTokens: 50,
		ErrorCount:   1,
		Invocations: []model.Invocation{
			{Input: model.Body{CacheRead: 10, CacheWrite: 2}},
			{Input: model.Body{CacheRead: 30, CacheWrite: 0}},
		},
	}
	s := mapStats(cs)
	if s.InvocationCount != 2 || s.InputTokens != 100 || s.OutputTokens != 50 || s.ErrorCount != 1 {
		t.Errorf("stats base counters = %+v", s)
	}
	if s.CacheReadTokens != 40 || s.CacheWriteTokens != 2 {
		t.Errorf("cache totals = read %d write %d, want 40/2", s.CacheReadTokens, s.CacheWriteTokens)
	}
}

func TestMapInvocationsEmbedsInlineBodies(t *testing.T) {
	invs := []model.Invocation{{
		RequestID:      "req-1",
		ModelID:        "gpt-4o",
		Operation:      "ChatCompletions_Create",
		Status:         "200",
		FullIdentity:   model.Identity{Principal: "oid-123", Extra: map[string]string{"region": "eastus"}},
		LatencyMs:      1200,
		Input:          model.Body{JSON: json.RawMessage(`{"messages":[]}`), ContentType: "application/json", TokenCount: 10, CacheRead: 4},
		Output:         model.Body{JSON: json.RawMessage(`{"choices":[]}`), ContentType: "application/json", TokenCount: 5},
		ProviderExtras: json.RawMessage(`{"correlationId":"abc"}`),
	}}

	out := mapInvocations(invs)
	if len(out) != 1 {
		t.Fatalf("invocations = %d, want 1", len(out))
	}
	got := out[0]
	if string(got.Input.JSON) != `{"messages":[]}` {
		t.Errorf("input body = %s, want inline verbatim", got.Input.JSON)
	}
	if got.Input.CacheRead != 4 {
		t.Errorf("input cache_read = %d, want 4", got.Input.CacheRead)
	}
	if got.Identity.Extra["region"] != "eastus" {
		t.Errorf("identity extra not carried: %+v", got.Identity.Extra)
	}
	if string(got.ProviderExtras) != `{"correlationId":"abc"}` {
		t.Errorf("provider_extras = %s, want inline verbatim", got.ProviderExtras)
	}
}

func TestInlineJSONDropsInvalid(t *testing.T) {
	if got := inlineJSON(json.RawMessage(`{not json`)); got != nil {
		t.Errorf("inlineJSON(invalid) = %s, want nil", got)
	}
	if got := inlineJSON(nil); got != nil {
		t.Errorf("inlineJSON(nil) = %s, want nil", got)
	}
	valid := json.RawMessage(`{"k":1}`)
	if got := inlineJSON(valid); string(got) != string(valid) {
		t.Errorf("inlineJSON(valid) = %s, want %s", got, valid)
	}
}
