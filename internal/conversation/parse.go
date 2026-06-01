// Package conversation reconstructs conversations from normalized invocation
// records, dispatching on each record's provider to the appropriate envelope
// parser (currently Anthropic Messages API request/response bodies).
package conversation

import (
	"encoding/base64"
	"encoding/json"
	"sort"
	"strings"

	"github.com/rxbynerd/dressage/internal/model"
)

// messagesAPIRequest represents the relevant fields of an Anthropic Messages API request body.
type messagesAPIRequest struct {
	System   json.RawMessage `json:"system"`
	Messages []apiMessage    `json:"messages"`
	Tools    []apiTool       `json:"tools"`
	Metadata struct {
		UserID string `json:"user_id"`
	} `json:"metadata"`
}

type apiMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or []contentBlock
}

type apiContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Name      string          `json:"name,omitempty"`
	ID        string          `json:"id,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // tool_result content
	IsError   bool            `json:"is_error,omitempty"`
	Source    *apiImageSource `json:"source,omitempty"` // image/document source (for "image"/"document")
}

// apiImageSource is the source of an Anthropic image/document content block:
// either base64-inlined bytes ({type:"base64", media_type, data}) or a URL
// reference ({type:"url", url}).
type apiImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url"`
}

type apiTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// extractSessionAnthropic parses an Anthropic Messages API request body and
// returns the session UUID if present. The session ID is extracted from
// metadata.user_id which has the format: user_{hash}_account__session_{uuid}
func extractSessionAnthropic(inputBody json.RawMessage) string {
	if len(inputBody) == 0 {
		return ""
	}
	var req struct {
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(inputBody, &req); err != nil {
		return ""
	}
	return sessionSuffix(req.Metadata.UserID)
}

// sessionSuffix extracts the session UUID that follows the "_session_" marker
// in an identity string of the form user_{hash}_account__session_{uuid} (as set
// by Claude Code). It returns "" when the marker is absent. Shared by the
// Anthropic and OpenAI session-extraction paths.
func sessionSuffix(s string) string {
	const prefix = "_session_"
	if idx := strings.Index(s, prefix); idx >= 0 {
		return s[idx+len(prefix):]
	}
	return ""
}

// parsedInvocation pairs a record with its parsed Messages API request body.
type parsedInvocation struct {
	rec *model.Record
	req *messagesAPIRequest
}

// reconstructAnthropic builds a ConversationDetail from a set of records
// belonging to the same conversation, decoded as Anthropic Messages API
// request/response bodies. It finds the invocation with the most messages
// (the latest main-thread turn), extracts the full conversation history from
// its input, and appends the final assistant response from its output.
func reconstructAnthropic(records []model.Record) *model.ConversationDetail {
	if len(records) == 0 {
		return nil
	}

	// Sort by timestamp for consistent ordering.
	sorted := make([]model.Record, len(records))
	copy(sorted, records)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Timestamp.Before(sorted[j].Timestamp)
	})

	// Parse all request bodies and find the one with the most messages
	// (the latest main-thread invocation).
	var all []parsedInvocation
	bestIdx := -1

	for i := range sorted {
		req := parseRequest(sorted[i].Input.JSON)
		if req == nil {
			continue
		}
		all = append(all, parsedInvocation{rec: &sorted[i], req: req})
		if bestIdx < 0 || len(req.Messages) > len(all[bestIdx].req.Messages) {
			bestIdx = len(all) - 1
		}
	}

	if bestIdx < 0 {
		return nil
	}
	// Resolve the pointer AFTER the loop so it does not alias into a slice that
	// may have been reallocated by append.
	best := &all[bestIdx]

	detail := &model.ConversationDetail{
		SessionID:    extractSessionFromReq(best.req),
		SystemPrompt: extractSystemPrompt(best.req.System),
		Tools:        extractTools(best.req.Tools),
	}

	// Convert all messages from the input into turns.
	for _, msg := range best.req.Messages {
		turn := convertMessage(msg)
		detail.Turns = append(detail.Turns, turn)
	}

	// Reassemble the final assistant response from the output body.
	finalTurn := reassembleOutput(best.rec.Output.JSON)
	if finalTurn != nil {
		finalTurn.Metrics = extractMetricsFromLog(best.rec, best.rec.Output.JSON)
		detail.Turns = append(detail.Turns, *finalTurn)
	}

	// Attach metrics to earlier assistant turns by matching invocations
	// to the turns they produced. Invocation with N input messages produced
	// the assistant turn at index N in the conversation.
	attachMetrics(detail, all)

	return detail
}

// attachMetrics correlates invocations with assistant turns and attaches
// per-invocation metrics. An invocation with N input messages produced the
// assistant turn at message index N.
func attachMetrics(detail *model.ConversationDetail, invocations []parsedInvocation) {
	for _, p := range invocations {
		turnIdx := len(p.req.Messages)
		if turnIdx >= len(detail.Turns) {
			continue // this is the final invocation, already handled
		}
		turn := &detail.Turns[turnIdx]
		if turn.Role != "assistant" {
			continue
		}
		if turn.Metrics != nil {
			continue // already has metrics
		}
		turn.Metrics = extractMetricsFromLog(p.rec, p.rec.Output.JSON)
	}
}

// extractMetricsFromLog builds TurnMetrics from the invocation record
// and the streaming output chunks.
func extractMetricsFromLog(rec *model.Record, outputBody json.RawMessage) *model.TurnMetrics {
	m := &model.TurnMetrics{
		Timestamp:        rec.Timestamp,
		RequestID:        rec.RequestID,
		ModelID:          rec.ModelID,
		InputTokens:      rec.Input.TokenCount,
		OutputTokens:     rec.Output.TokenCount,
		CacheReadTokens:  rec.Input.CacheRead,
		CacheWriteTokens: rec.Input.CacheWrite,
	}

	// Try to extract additional metrics from the streaming output chunks.
	sm := extractStreamMetrics(outputBody)
	if sm != nil {
		m.LatencyMs = sm.LatencyMs
		m.FirstByteMs = sm.FirstByteMs
		m.StopReason = sm.StopReason
		if sm.CacheReadTokens > 0 {
			m.CacheReadTokens = sm.CacheReadTokens
		}
		if sm.CacheWriteTokens > 0 {
			m.CacheWriteTokens = sm.CacheWriteTokens
		}
	}

	return m
}

func parseRequest(body json.RawMessage) *messagesAPIRequest {
	if len(body) == 0 {
		return nil
	}
	var req messagesAPIRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}
	// Require at least one message to be a valid Messages API request.
	if len(req.Messages) == 0 {
		return nil
	}
	return &req
}

func extractSessionFromReq(req *messagesAPIRequest) string {
	return sessionSuffix(req.Metadata.UserID)
}

// extractSystemPrompt handles both string and array formats for the system field.
func extractSystemPrompt(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	// Try as string first.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}

	// Try as array of text blocks.
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n\n")
	}

	return string(raw)
}

// extractTools maps Anthropic tools[] to model.ToolDef, carrying the full
// description and the input_schema verbatim. Descriptions are NOT truncated
// here: truncation is a presentation concern handled at render time.
func extractTools(tools []apiTool) []model.ToolDef {
	result := make([]model.ToolDef, len(tools))
	for i, t := range tools {
		result[i] = model.ToolDef{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		}
	}
	return result
}

func convertMessage(msg apiMessage) model.Turn {
	turn := model.Turn{Role: msg.Role}

	// Content can be a plain string or an array of content blocks.
	var str string
	if err := json.Unmarshal(msg.Content, &str); err == nil {
		if str != "" {
			turn.Blocks = append(turn.Blocks, model.ContentBlock{
				Type: "text",
				Text: str,
			})
		}
		return turn
	}

	var blocks []apiContentBlock
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		// Fallback: treat as raw text.
		turn.Blocks = append(turn.Blocks, model.ContentBlock{
			Type: "text",
			Text: string(msg.Content),
		})
		return turn
	}

	for _, b := range blocks {
		turn.Blocks = append(turn.Blocks, convertContentBlock(b))
	}
	return turn
}

func convertContentBlock(b apiContentBlock) model.ContentBlock {
	switch b.Type {
	case "text":
		return model.ContentBlock{Type: "text", Text: b.Text}
	case "thinking":
		return model.ContentBlock{Type: "thinking", Text: b.Thinking}
	case "tool_use":
		return model.ContentBlock{
			Type:      "tool_use",
			ToolName:  b.Name,
			ToolID:    b.ID,
			ToolInput: prettyJSON(b.Input),
		}
	case "tool_result":
		return model.ContentBlock{
			Type:          "tool_result",
			ToolID:        b.ToolUseID,
			ResultContent: extractToolResultContent(b.Content),
			IsError:       b.IsError,
		}
	case "image", "document":
		return imageSourceBlock(b.Source)
	default:
		return model.ContentBlock{Type: b.Type, Text: string(b.Content)}
	}
}

// imageSourceBlock builds a "media" content block from an Anthropic image/document
// source. A base64 source is inline (bytes stay in the raw body; only the decoded
// length is recorded); a url source carries the external reference.
func imageSourceBlock(src *apiImageSource) model.ContentBlock {
	block := model.ContentBlock{Type: "media"}
	if src == nil {
		return block
	}
	block.MimeType = src.MediaType
	switch src.Type {
	case "url":
		block.FileURI = src.URL
	case "base64":
		block.MediaInline = true
		block.MediaBytes = base64DecodedLen(src.Data)
	default:
		if src.URL != "" {
			block.FileURI = src.URL
		} else if src.Data != "" {
			block.MediaInline = true
			block.MediaBytes = base64DecodedLen(src.Data)
		}
	}
	return block
}

// extractToolResultContent handles both string and array formats for tool_result content.
func extractToolResultContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	// Try as string.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}

	// Try as array of content blocks.
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}

	return string(raw)
}

func prettyJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	pretty, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return string(raw)
	}
	return string(pretty)
}

// base64DecodedLen returns the number of bytes a standard base64 string decodes
// to, without allocating the decoded buffer. It returns 0 for invalid input so
// the caller records an unknown size rather than a misleading one.
func base64DecodedLen(s string) int64 {
	if s == "" {
		return 0
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return 0
	}
	return int64(len(b))
}
