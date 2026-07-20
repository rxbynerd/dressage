package model

import (
	"encoding/json"
	"time"
)

// Report is the top-level grouped view of a run: aggregate stats plus the
// per-day, per-conversation breakdown the IR exporter consumes.
type Report struct {
	GeneratedAt time.Time
	DateRange   DateRange
	TotalStats  Stats
	Days        []DaySummary
}

// DateRange represents the start and end dates of the report.
type DateRange struct {
	Start time.Time
	End   time.Time
}

// Stats holds aggregate statistics. Cache counters follow each provider's own
// accounting (Anthropic/Bedrock report them alongside InputTokens; OpenAI and
// Gemini report cache reads as a subset of the prompt count), so they are
// carried as separate totals rather than folded into InputTokens.
type Stats struct {
	InvocationCount  int
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ErrorCount       int
	ModelBreakdown   map[string]int
	OpBreakdown      map[string]int
}

// DaySummary holds the summary for a single day.
type DaySummary struct {
	Date          time.Time
	Stats         Stats
	Conversations []ConversationSummary
}

// ConversationSummary groups related invocations into a logical conversation.
type ConversationSummary struct {
	ID           string
	SessionID    string // session id extracted from the request body when present
	Provider     string // provider id shared by all records in the conversation
	ModelID      string
	Identity     string
	StartTime    time.Time
	EndTime      time.Time
	MessageCount int
	InputTokens  int64
	OutputTokens int64
	ErrorCount   int
	Invocations  []Invocation
	Detail       *ConversationDetail // reconstructed main-thread conversation (nil if not available)
	Sidechains   []Thread            // reconstructed sidechain threads (subagents); empty when none or when Detail is nil
}

// Invocation is a single request/response pair, preserved verbatim so the IR
// exporter can embed inline JSON bodies and full per-invocation metadata
// without re-fetching the source logs. Token/cache accounting and the
// normalized identity live on the raw fields (Input/Output, FullIdentity).
type Invocation struct {
	Timestamp time.Time
	RequestID string
	ModelID   string
	Operation string
	Status    string
	ErrorCode string

	LatencyMs      int64
	StopReason     string      // response stop/finish reason, when the fetcher lifted it
	Correlation    Correlation // provider-assigned message/thread linkage, when available
	FullIdentity   Identity
	Input          Body            // raw input body + token/cache accounting
	Output         Body            // raw output body + token/cache accounting
	ProviderExtras json.RawMessage // opaque per-provider fields, if present
}
