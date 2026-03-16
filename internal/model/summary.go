package model

import "time"

// Report is the top-level structure passed to the HTML template.
type Report struct {
	GeneratedAt    time.Time
	DateRange      DateRange
	TotalStats     Stats
	Days           []DaySummary
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
	SessionID    string // Claude Code session UUID (from metadata.user_id)
	ModelID      string
	IdentityARN  string
	StartTime    time.Time
	EndTime      time.Time
	MessageCount int
	InputTokens  int64
	OutputTokens int64
	ErrorCount   int
	Invocations  []Invocation
	Detail       *ConversationDetail // reconstructed conversation (nil if not available)
}

// Invocation is a single request/response pair, ready for display.
type Invocation struct {
	Timestamp    time.Time
	RequestID    string
	ModelID      string
	Operation    string
	Status       string
	ErrorCode    string
	InputBody    string // pretty-printed JSON
	OutputBody   string // pretty-printed JSON
	InputTokens  int64
	OutputTokens int64
	IdentityARN  string
}
