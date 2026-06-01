package ir

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/rxbynerd/dressage/internal/model"
)

// rfc3339 is the timestamp layout used for stable-id derivation. It must stay
// fixed: changing it would change every hashed id.
const rfc3339 = time.RFC3339

// mapConversation translates a model.ConversationSummary into a ConversationIR.
// It is pure (no IO): all translation logic lives here so Export only has to
// concern itself with writing files.
func mapConversation(cs model.ConversationSummary) ConversationIR {
	conv := ConversationIR{
		SchemaVersion: SchemaVersion,
		ID:            stableID(cs),
		DisplayID:     cs.ID,
		SessionID:     cs.SessionID,
		Provider:      cs.Provider,
		ModelID:       cs.ModelID,
		Identity:      conversationIdentity(cs),
		StartTime:     cs.StartTime,
		EndTime:       cs.EndTime,
		Stats:         mapStats(cs),
		Invocations:   mapInvocations(cs.Invocations),
	}
	if cs.Detail != nil {
		conv.Conversation = mapDetail(cs.Detail)
	}
	return conv
}

// mapEntry builds the lightweight manifest index entry for a conversation. It
// takes the already-mapped ConversationIR so the id, file name, and counts stay
// consistent with the conversation file.
func mapEntry(conv ConversationIR) ManifestEntry {
	entry := ManifestEntry{
		ID:              conv.ID,
		File:            conversationFile(conv.ID),
		Provider:        conv.Provider,
		ModelID:         conv.ModelID,
		SessionID:       conv.SessionID,
		StartTime:       conv.StartTime,
		EndTime:         conv.EndTime,
		InvocationCount: conv.Stats.InvocationCount,
		InputTokens:     conv.Stats.InputTokens,
		OutputTokens:    conv.Stats.OutputTokens,
		ErrorCount:      conv.Stats.ErrorCount,
		Reconstructed:   conv.Conversation != nil,
	}
	if conv.Conversation != nil {
		entry.TurnCount = len(conv.Conversation.Turns)
	}
	return entry
}

// conversationFile returns the manifest-relative path of a conversation's file.
func conversationFile(id string) string {
	return "conversations/" + id + ".json"
}

// stableID derives a stable, run-order-independent id for a conversation. If a
// session id is present it is used verbatim (already stable and provider-unique
// via the (provider, session) grouping key). Otherwise the id is the first 16
// hex chars of SHA-256 over (provider, model_id, principal, start_time RFC3339),
// which is deterministic for the same underlying conversation regardless of run
// order or sibling conversations.
func stableID(cs model.ConversationSummary) string {
	if cs.SessionID != "" {
		return cs.SessionID
	}
	h := sha256.New()
	// Join with a NUL separator so distinct field boundaries cannot collide
	// (e.g. provider "ab"+model "c" vs provider "a"+model "bc").
	for _, part := range []string{cs.Provider, cs.ModelID, cs.Identity, cs.StartTime.UTC().Format(rfc3339)} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// conversationIdentity picks the conversation's identity from its first
// invocation's full record identity, falling back to the principal-only string
// carried on the summary.
func conversationIdentity(cs model.ConversationSummary) IdentityIR {
	if len(cs.Invocations) > 0 {
		return mapIdentity(cs.Invocations[0].FullIdentity)
	}
	return IdentityIR{Principal: cs.Identity}
}

// mapStats aggregates the per-conversation counters. Token and error totals come
// from the summary; cache totals are summed from the raw invocation bodies,
// which the summary does not pre-aggregate.
func mapStats(cs model.ConversationSummary) StatsIR {
	s := StatsIR{
		InvocationCount: cs.MessageCount,
		InputTokens:     cs.InputTokens,
		OutputTokens:    cs.OutputTokens,
		ErrorCount:      cs.ErrorCount,
	}
	for _, inv := range cs.Invocations {
		s.CacheReadTokens += inv.Input.CacheRead
		s.CacheWriteTokens += inv.Input.CacheWrite
	}
	return s
}

// mapDetail translates a reconstructed ConversationDetail into the IR view.
func mapDetail(d *model.ConversationDetail) *ConversationView {
	view := &ConversationView{
		SystemPrompt: d.SystemPrompt,
		Tools:        mapTools(d.Tools),
		Turns:        make([]TurnIR, 0, len(d.Turns)),
	}
	for _, t := range d.Turns {
		view.Turns = append(view.Turns, mapTurn(t))
	}
	return view
}

// mapTools translates tool definitions, carrying full descriptions and the
// inline input schema.
func mapTools(tools []model.ToolDef) []ToolIR {
	if len(tools) == 0 {
		return nil
	}
	out := make([]ToolIR, len(tools))
	for i, t := range tools {
		out[i] = ToolIR{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: inlineJSON(t.InputSchema),
		}
	}
	return out
}

// mapTurn translates one reconstructed turn.
func mapTurn(t model.Turn) TurnIR {
	turn := TurnIR{
		Role:   t.Role,
		Blocks: make([]BlockIR, 0, len(t.Blocks)),
	}
	for _, b := range t.Blocks {
		turn.Blocks = append(turn.Blocks, mapBlock(b))
	}
	if t.Metrics != nil {
		turn.Metrics = mapMetrics(t.Metrics)
	}
	return turn
}

// mapBlock translates one content block, populating only the fields relevant to
// its type. tool_input is re-parsed into inline JSON; an unparseable input is
// preserved as a JSON string so the field is always valid JSON.
func mapBlock(b model.ContentBlock) BlockIR {
	block := BlockIR{Type: b.Type}
	switch b.Type {
	case "text", "thinking":
		block.Text = b.Text
	case "tool_use":
		block.ToolID = b.ToolID
		block.ToolName = b.ToolName
		block.ToolInput = toolInputJSON(b.ToolInput)
	case "tool_result":
		block.ToolID = b.ToolID
		block.IsError = b.IsError
		block.ResultContent = b.ResultContent
	case "media":
		block.MimeType = b.MimeType
		block.FileURI = b.FileURI
		block.Inline = b.MediaInline
		block.ByteSize = b.MediaBytes
	default:
		// Unrecognized provider types pass through with a best-effort text
		// payload (mirrors how the reconstructors surface unknown blocks).
		block.Text = b.Text
	}
	return block
}

// mapMetrics translates per-turn metrics.
func mapMetrics(m *model.TurnMetrics) *MetricsIR {
	return &MetricsIR{
		Timestamp:        m.Timestamp,
		RequestID:        m.RequestID,
		ModelID:          m.ModelID,
		InputTokens:      m.InputTokens,
		OutputTokens:     m.OutputTokens,
		CacheReadTokens:  m.CacheReadTokens,
		CacheWriteTokens: m.CacheWriteTokens,
		LatencyMs:        m.LatencyMs,
		FirstByteMs:      m.FirstByteMs,
		StopReason:       m.StopReason,
	}
}

// mapInvocations translates the raw invocations, embedding bodies inline.
func mapInvocations(invs []model.Invocation) []InvocationIR {
	out := make([]InvocationIR, 0, len(invs))
	for _, inv := range invs {
		out = append(out, InvocationIR{
			Timestamp:      inv.Timestamp,
			RequestID:      inv.RequestID,
			ModelID:        inv.ModelID,
			Operation:      inv.Operation,
			Status:         inv.Status,
			ErrorCode:      inv.ErrorCode,
			Identity:       mapIdentity(inv.FullIdentity),
			LatencyMs:      inv.LatencyMs,
			Input:          mapBody(inv.Input),
			Output:         mapBody(inv.Output),
			ProviderExtras: inlineJSON(inv.ProviderExtras),
		})
	}
	return out
}

// mapBody translates a body, embedding its raw JSON inline.
func mapBody(b model.Body) BodyIR {
	return BodyIR{
		ContentType: b.ContentType,
		TokenCount:  b.TokenCount,
		CacheRead:   b.CacheRead,
		CacheWrite:  b.CacheWrite,
		JSON:        inlineJSON(b.JSON),
	}
}

// mapIdentity translates a normalized identity.
func mapIdentity(id model.Identity) IdentityIR {
	return IdentityIR{
		Principal: id.Principal,
		Display:   id.Display,
		Extra:     id.Extra,
	}
}

// inlineJSON returns raw as inline JSON when it is valid, else nil. This keeps a
// conversation file itself valid JSON: an unparseable or absent raw body is
// dropped (its omitempty field disappears) rather than corrupting the document.
func inlineJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	if !json.Valid(raw) {
		return nil
	}
	return raw
}

// toolInputJSON converts a pretty-printed tool-input string back into inline
// JSON. When the string is not valid JSON (e.g. a provider sent a non-JSON
// argument blob), it is preserved as a JSON string so the field stays valid.
func toolInputJSON(s string) json.RawMessage {
	if s == "" {
		return nil
	}
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	encoded, err := json.Marshal(s)
	if err != nil {
		return nil
	}
	return encoded
}
