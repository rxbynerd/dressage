package conversation

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/rxbynerd/dressage/internal/model"
)

// openaiRequest represents the relevant fields of an OpenAI Chat Completions
// request body (the envelope Azure OpenAI surfaces).
type openaiRequest struct {
	Messages []openaiMessage `json:"messages"`
	Tools    []openaiTool    `json:"tools"`
	User     string          `json:"user"`
	Metadata struct {
		UserID string `json:"user_id"`
	} `json:"metadata"`
}

// openaiMessage is one entry in messages[]. Content is a string OR an array of
// parts; tool_calls / function_call appear on assistant messages; tool_call_id
// appears on tool-role messages.
type openaiMessage struct {
	Role         string              `json:"role"`
	Content      json.RawMessage     `json:"content"` // string, []part, or null
	ToolCalls    []openaiToolCall    `json:"tool_calls"`
	FunctionCall *openaiFunctionCall `json:"function_call"`
	ToolCallID   string              `json:"tool_call_id"`
}

// openaiToolCall is a new-style tool call: {id, type:"function", function:{name, arguments}}.
type openaiToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openaiFunctionCall `json:"function"`
}

// openaiFunctionCall is the function payload of a tool call (or the legacy
// top-level function_call). Arguments is a STRINGIFIED JSON blob.
type openaiFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// openaiTool is one entry in tools[]: {type:"function", function:{name, description, ...}}.
type openaiTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"function"`
}

// openaiContentPart is one element of an array-form message content.
type openaiContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url"`
}

// parsedOpenAIInvocation pairs a record with its parsed OpenAI request body.
type parsedOpenAIInvocation struct {
	rec *model.Record
	req *openaiRequest
}

// reconstructOpenAI builds a ConversationDetail from a set of records belonging
// to the same conversation, decoded as OpenAI Chat Completions request/response
// bodies. It mirrors reconstructAnthropic's structure: find the invocation with
// the most messages, expand its request into turns, append the final assistant
// response from its output, and attach per-invocation metrics to earlier turns.
func reconstructOpenAI(records []model.Record) *model.ConversationDetail {
	if len(records) == 0 {
		return nil
	}

	sorted := make([]model.Record, len(records))
	copy(sorted, records)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Timestamp.Before(sorted[j].Timestamp)
	})

	var all []parsedOpenAIInvocation
	var best *parsedOpenAIInvocation
	for i := range sorted {
		req := parseOpenAIRequest(sorted[i].Input.JSON)
		if req == nil {
			continue
		}
		p := parsedOpenAIInvocation{rec: &sorted[i], req: req}
		all = append(all, p)
		if best == nil || len(req.Messages) > len(best.req.Messages) {
			best = &all[len(all)-1]
		}
	}

	if best == nil {
		return nil
	}

	detail := &model.ConversationDetail{
		SessionID:    extractSessionFromUser(best.req.User, best.req.Metadata.UserID),
		SystemPrompt: openaiSystemPrompt(best.req.Messages),
		Tools:        openaiTools(best.req.Tools),
	}

	// Expand non-system messages into turns (system/developer become SystemPrompt).
	for _, msg := range best.req.Messages {
		if isSystemRole(msg.Role) {
			continue
		}
		detail.Turns = append(detail.Turns, openaiTurn(msg))
	}

	// Append the final assistant response from the output body.
	finalTurn := reassembleOpenAIOutput(best.rec.Output.JSON)
	if finalTurn != nil {
		finalTurn.Metrics = openaiMetrics(best.rec, best.rec.Output.JSON)
		detail.Turns = append(detail.Turns, *finalTurn)
	}

	attachOpenAIMetrics(detail, all)

	return detail
}

// attachOpenAIMetrics correlates earlier invocations with the assistant turns
// they produced. Because system/developer messages are stripped from turns and
// every non-system message maps 1:1 to a turn, the assistant turn produced by an
// invocation sits at index = (number of non-system messages in its request).
func attachOpenAIMetrics(detail *model.ConversationDetail, invocations []parsedOpenAIInvocation) {
	for _, p := range invocations {
		turnIdx := nonSystemMessageCount(p.req.Messages)
		if turnIdx >= len(detail.Turns) {
			continue // final invocation, already handled
		}
		turn := &detail.Turns[turnIdx]
		if turn.Role != "assistant" {
			continue
		}
		if turn.Metrics != nil {
			continue
		}
		turn.Metrics = openaiMetrics(p.rec, p.rec.Output.JSON)
	}
}

// openaiMetrics builds TurnMetrics from the record plus the response usage and
// finish_reason. cached_tokens maps to CacheReadTokens; OpenAI has no
// cache-write counter so CacheWriteTokens stays 0.
func openaiMetrics(rec *model.Record, outputBody json.RawMessage) *model.TurnMetrics {
	m := &model.TurnMetrics{
		Timestamp:       rec.Timestamp,
		RequestID:       rec.RequestID,
		ModelID:         rec.ModelID,
		InputTokens:     rec.Input.TokenCount,
		OutputTokens:    rec.Output.TokenCount,
		CacheReadTokens: rec.Input.CacheRead,
		LatencyMs:       rec.LatencyMs,
	}

	resp := parseOpenAIResponse(outputBody)
	if resp != nil {
		if resp.Usage != nil {
			if resp.Usage.PromptTokens > 0 {
				m.InputTokens = resp.Usage.PromptTokens
			}
			if resp.Usage.CompletionTokens > 0 {
				m.OutputTokens = resp.Usage.CompletionTokens
			}
			if resp.Usage.PromptTokensDetails.CachedTokens > 0 {
				m.CacheReadTokens = resp.Usage.PromptTokensDetails.CachedTokens
			}
		}
		if len(resp.Choices) > 0 {
			m.StopReason = resp.Choices[0].FinishReason
		}
	}

	return m
}

func parseOpenAIRequest(body json.RawMessage) *openaiRequest {
	if len(body) == 0 {
		return nil
	}
	var req openaiRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}
	if len(req.Messages) == 0 {
		return nil
	}
	return &req
}

func isSystemRole(role string) bool {
	return role == "system" || role == "developer"
}

func nonSystemMessageCount(messages []openaiMessage) int {
	n := 0
	for _, m := range messages {
		if !isSystemRole(m.Role) {
			n++
		}
	}
	return n
}

// openaiSystemPrompt concatenates the text of all system/developer messages,
// joined with a blank line. These messages are NOT emitted as turns.
func openaiSystemPrompt(messages []openaiMessage) string {
	var parts []string
	for _, m := range messages {
		if !isSystemRole(m.Role) {
			continue
		}
		if text := contentText(m.Content); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// openaiTools maps tools[].function to model.ToolDef, truncating long
// descriptions for display (matching the Anthropic path).
func openaiTools(tools []openaiTool) []model.ToolDef {
	if len(tools) == 0 {
		return nil
	}
	result := make([]model.ToolDef, len(tools))
	for i, t := range tools {
		desc := t.Function.Description
		if len(desc) > 200 {
			desc = desc[:200] + "..."
		}
		result[i] = model.ToolDef{Name: t.Function.Name, Description: desc}
	}
	return result
}

// openaiTurn converts one non-system OpenAI message into a Turn.
func openaiTurn(msg openaiMessage) model.Turn {
	switch msg.Role {
	case "tool":
		// Tool results render as user turns (consistent with the Anthropic
		// tool_result blocks, which live in user turns).
		return model.Turn{
			Role: "user",
			Blocks: []model.ContentBlock{{
				Type:          "tool_result",
				ToolID:        msg.ToolCallID,
				ResultContent: contentText(msg.Content),
			}},
		}
	case "assistant":
		return model.Turn{Role: "assistant", Blocks: assistantBlocks(msg.Content, msg.ToolCalls, msg.FunctionCall)}
	default: // user (and any unrecognized role rendered verbatim)
		return model.Turn{Role: msg.Role, Blocks: userBlocks(msg.Content)}
	}
}

// userBlocks builds content blocks for a user message: a string becomes one
// text block; an array becomes a text block per text part, with image parts
// surfaced as a "[image]" text block rather than dropped silently.
func userBlocks(content json.RawMessage) []model.ContentBlock {
	if len(content) == 0 {
		return nil
	}

	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		if s == "" {
			return nil
		}
		return []model.ContentBlock{{Type: "text", Text: s}}
	}

	var parts []openaiContentPart
	if err := json.Unmarshal(content, &parts); err != nil {
		return []model.ContentBlock{{Type: "text", Text: string(content)}}
	}

	var blocks []model.ContentBlock
	for _, p := range parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				blocks = append(blocks, model.ContentBlock{Type: "text", Text: p.Text})
			}
		case "image_url":
			blocks = append(blocks, model.ContentBlock{Type: "text", Text: "[image]"})
		default:
			if p.Text != "" {
				blocks = append(blocks, model.ContentBlock{Type: "text", Text: p.Text})
			}
		}
	}
	return blocks
}

// assistantBlocks builds content blocks for an assistant message: optional text
// content, then a tool_use block per tool_call, then the legacy function_call.
func assistantBlocks(content json.RawMessage, toolCalls []openaiToolCall, functionCall *openaiFunctionCall) []model.ContentBlock {
	var blocks []model.ContentBlock

	if text := contentText(content); text != "" {
		blocks = append(blocks, model.ContentBlock{Type: "text", Text: text})
	}

	for _, tc := range toolCalls {
		blocks = append(blocks, model.ContentBlock{
			Type:      "tool_use",
			ToolName:  tc.Function.Name,
			ToolID:    tc.ID,
			ToolInput: prettyJSON(json.RawMessage(tc.Function.Arguments)),
		})
	}

	if functionCall != nil && functionCall.Name != "" {
		blocks = append(blocks, model.ContentBlock{
			Type:      "tool_use",
			ToolName:  functionCall.Name,
			ToolInput: prettyJSON(json.RawMessage(functionCall.Arguments)),
		})
	}

	return blocks
}

// contentText flattens an OpenAI message content (string, array of parts, or
// null) into plain text. Image parts contribute "[image]".
func contentText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}

	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s
	}

	var parts []openaiContentPart
	if err := json.Unmarshal(content, &parts); err == nil {
		var sb []string
		for _, p := range parts {
			switch {
			case p.Type == "image_url" || p.ImageURL != nil:
				sb = append(sb, "[image]")
			case p.Text != "":
				sb = append(sb, p.Text)
			}
		}
		return strings.Join(sb, "\n")
	}

	return string(content)
}

// extractSessionFromUser applies the shared _session_ suffix extraction to the
// top-level `user` field, falling back to metadata.user_id.
func extractSessionFromUser(user, metadataUserID string) string {
	if sid := sessionSuffix(user); sid != "" {
		return sid
	}
	return sessionSuffix(metadataUserID)
}

// extractSessionOpenAI parses an OpenAI request body and returns the session id
// from the top-level `user` field (falling back to metadata.user_id), applying
// the same _session_ suffix extraction as the Anthropic path. Returns "" when no
// _session_ marker is present.
func extractSessionOpenAI(inputBody json.RawMessage) string {
	if len(inputBody) == 0 {
		return ""
	}
	var req struct {
		User     string `json:"user"`
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(inputBody, &req); err != nil {
		return ""
	}
	return extractSessionFromUser(req.User, req.Metadata.UserID)
}
