package model

import (
	"encoding/json"
	"time"
)

// Report is the top-level structure passed to the HTML template.
type Report struct {
	Title       string // report heading, e.g. "Bedrock Invocation Log Report"
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

// Stats holds aggregate statistics.
type Stats struct {
	InvocationCount int
	InputTokens     int64
	OutputTokens    int64
	ErrorCount      int
	ModelBreakdown  map[string]int
	OpBreakdown     map[string]int
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

// Invocation is a single request/response pair. It carries both a display copy
// (pretty-printed JSON bodies, principal-only identity) used by the HTML report
// and the raw record fields (inline JSON bodies, cache counts, full identity,
// provider extras) needed for a faithful machine-readable export.
type Invocation struct {
	Timestamp    time.Time
	RequestID    string
	ModelID      string
	Operation    string
	Status       string
	ErrorCode    string
	InputBody    string // pretty-printed JSON (for HTML display)
	OutputBody   string // pretty-printed JSON (for HTML display)
	InputTokens  int64
	OutputTokens int64
	Identity     string // principal only (for HTML display)

	// Raw record fields, preserved verbatim for faithful export. These are not
	// used by the HTML report but let the IR exporter embed inline JSON bodies
	// and full per-invocation metadata without re-fetching the source logs.
	LatencyMs      int64
	StopReason     string      // response stop/finish reason, when the fetcher lifted it
	Correlation    Correlation // provider-assigned message/thread linkage, when available
	FullIdentity   Identity
	Input          Body            // raw input body + token/cache accounting
	Output         Body            // raw output body + token/cache accounting
	ProviderExtras json.RawMessage // opaque per-provider fields, if present
}
