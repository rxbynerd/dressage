package model

import "time"

// ConversationDetail holds a fully reconstructed conversation parsed from
// the Messages API request/response bodies in Bedrock invocation logs.
type ConversationDetail struct {
	SessionID    string
	SystemPrompt string
	Tools        []ToolDef
	Turns        []Turn
}

// Turn is a single message in a conversation (user or assistant).
type Turn struct {
	Role    string         // "user" or "assistant"
	Blocks  []ContentBlock // content blocks within this turn
	Metrics *TurnMetrics   // non-nil for assistant turns
}

// HasText returns true if the turn contains at least one text block.
func (t Turn) HasText() bool {
	for _, b := range t.Blocks {
		if b.Type == "text" && b.Text != "" {
			return true
		}
	}
	return false
}

// HasToolUse returns true if the turn contains at least one tool_use block.
func (t Turn) HasToolUse() bool {
	for _, b := range t.Blocks {
		if b.Type == "tool_use" {
			return true
		}
	}
	return false
}

// ContentBlock is a single content element within a turn.
type ContentBlock struct {
	Type          string // "text", "tool_use", "tool_result", "thinking"
	Text          string // text content (for "text" and "thinking" types)
	ToolName      string // tool name (for "tool_use")
	ToolID        string // tool use ID (for "tool_use" and "tool_result")
	ToolInput     string // pretty-printed JSON input (for "tool_use")
	ResultContent string // result text (for "tool_result")
	IsError       bool   // whether tool execution errored (for "tool_result")
}

// TurnMetrics contains per-invocation performance data for an assistant turn.
type TurnMetrics struct {
	Timestamp        time.Time
	RequestID        string
	ModelID          string
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	LatencyMs        int64
	FirstByteMs      int64
	StopReason       string
}

// ToolDef describes a tool available in the conversation.
type ToolDef struct {
	Name        string
	Description string
}
