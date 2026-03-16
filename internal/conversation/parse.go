// Package conversation reconstructs Claude Code conversations from Bedrock
// model invocation logs containing Anthropic Messages API request/response bodies.
package conversation

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/rubynerd/dressage/internal/model"
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
}

type apiTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ExtractSessionID parses the inputBodyJson and returns the session UUID if present.
// The session ID is extracted from metadata.user_id which has the format:
// user_{hash}_account__session_{uuid}
func ExtractSessionID(inputBody json.RawMessage) string {
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
	const prefix = "_session_"
	if idx := strings.Index(req.Metadata.UserID, prefix); idx >= 0 {
		return req.Metadata.UserID[idx+len(prefix):]
	}
	return ""
}

// parsedInvocation pairs a log entry with its parsed Messages API request body.
type parsedInvocation struct {
	log *model.InvocationLog
	req *messagesAPIRequest
}

// Reconstruct builds a ConversationDetail from a set of invocation logs
// belonging to the same conversation. It finds the invocation with the most
// messages (the latest main-thread turn), extracts the full conversation
// history from its input, and appends the final assistant response from its output.
func Reconstruct(logs []model.InvocationLog) *model.ConversationDetail {
	if len(logs) == 0 {
		return nil
	}

	// Sort by timestamp for consistent ordering.
	sorted := make([]model.InvocationLog, len(logs))
	copy(sorted, logs)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Timestamp.Before(sorted[j].Timestamp)
	})

	// Parse all request bodies and find the one with the most messages
	// (the latest main-thread invocation).
	var all []parsedInvocation
	var best *parsedInvocation

	for i := range sorted {
		req := parseRequest(sorted[i].Input.InputBodyJSON)
		if req == nil {
			continue
		}
		p := parsedInvocation{log: &sorted[i], req: req}
		all = append(all, p)
		if best == nil || len(req.Messages) > len(best.req.Messages) {
			best = &all[len(all)-1]
		}
	}

	if best == nil {
		return nil
	}

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
	finalTurn := reassembleOutput(best.log.Output.OutputBodyJSON)
	if finalTurn != nil {
		finalTurn.Metrics = extractMetricsFromLog(best.log, best.log.Output.OutputBodyJSON)
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
		turn.Metrics = extractMetricsFromLog(p.log, p.log.Output.OutputBodyJSON)
	}
}

// extractMetricsFromLog builds TurnMetrics from the invocation log entry
// and the streaming output chunks.
func extractMetricsFromLog(log *model.InvocationLog, outputBody json.RawMessage) *model.TurnMetrics {
	m := &model.TurnMetrics{
		Timestamp:        log.Timestamp,
		RequestID:        log.RequestID,
		ModelID:          log.ModelID,
		InputTokens:      log.Input.InputTokenCount,
		OutputTokens:     log.Output.OutputTokenCount,
		CacheReadTokens:  log.Input.CacheReadInputTokenCount,
		CacheWriteTokens: log.Input.CacheWriteInputTokenCount,
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
	const prefix = "_session_"
	uid := req.Metadata.UserID
	if idx := strings.Index(uid, prefix); idx >= 0 {
		return uid[idx+len(prefix):]
	}
	return ""
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

func extractTools(tools []apiTool) []model.ToolDef {
	result := make([]model.ToolDef, len(tools))
	for i, t := range tools {
		desc := t.Description
		// Truncate long descriptions for display.
		if len(desc) > 200 {
			desc = desc[:200] + "..."
		}
		result[i] = model.ToolDef{Name: t.Name, Description: desc}
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
	default:
		return model.ContentBlock{Type: b.Type, Text: string(b.Content)}
	}
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
