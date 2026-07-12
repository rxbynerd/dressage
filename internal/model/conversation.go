package model

import (
	"encoding/json"
	"time"
)

// ConversationDetail holds a fully reconstructed conversation parsed from
// the request/response bodies of a provider's invocation records.
type ConversationDetail struct {
	SessionID    string
	SystemPrompt string
	Tools        []ToolDef
	Turns        []Turn
}

// Thread is one reconstructed sidechain of a conversation: a linear request
// chain within the same session (e.g. a subagent dispatched by the main
// thread) with its own transcript.
type Thread struct {
	ID     string // provider-assigned thread id (the chain root's request uuid)
	Detail *ConversationDetail
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
	// Type identifies the block kind. The known set ("text", "tool_use",
	// "tool_result", "thinking", "media") is NOT exhaustive: unrecognized
	// provider block types are passed through verbatim — readers must not assume
	// the set is closed.
	Type          string
	Text          string // text content (for "text" and "thinking" types)
	ToolName      string // tool name (for "tool_use")
	ToolID        string // tool use ID (for "tool_use" and "tool_result")
	ToolInput     string // pretty-printed JSON input (for "tool_use")
	ResultContent string // result text (for "tool_result")
	IsError       bool   // whether tool execution errored (for "tool_result")

	// Media fields (for "media"): metadata about an image/file/audio part. The
	// raw bytes are NOT inlined here — they remain in the invocation's raw body
	// — only the metadata needed to identify and reference the part is carried.
	MimeType    string // declared MIME type (for "media"), may be empty
	FileURI     string // external file reference, e.g. a gs:// URI (for "media")
	MediaInline bool   // true when the bytes were embedded inline in the body (for "media")
	MediaBytes  int64  // size in bytes of inline media, when known (for "media", 0 if unknown)
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
	Description string          // full, untruncated tool description
	InputSchema json.RawMessage // full tool input/parameters JSON schema (nil if absent)
}
