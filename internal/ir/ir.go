// Package ir defines the machine-readable Intermediate Representation (IR) of a
// Dressage report: a stable, versioned, provider-neutral JSON schema that a
// downstream analysis program consumes instead of re-fetching or re-parsing the
// provider-native logs. The IR branches off the same *model.Report as the HTML
// report (parallel to internal/report) and is written as a directory of one
// JSON file per conversation plus a manifest index (see Export).
//
// Field names are snake_case throughout (idiomatic for the cross-language
// consumers we expect). Raw provider bodies embed as inline JSON
// (json.RawMessage), not stringified, so a conversation file is itself valid
// JSON and a consumer can walk into a request body directly.
package ir

import (
	"encoding/json"
	"time"
)

// SchemaVersion is the single source-of-truth IR schema identifier, embedded in
// every emitted file. It follows "dressage.ir/MAJOR.MINOR": additive,
// backward-compatible changes bump MINOR; breaking changes bump MAJOR.
// Consumers should accept any matching MAJOR and ignore unknown fields.
//
// 1.1: raw request/response bodies became opt-in (manifest raw_bodies records
// the choice; the body json fields were always optional).
// 1.2: reconstructed sidechains (subagent threads) are carried in each
// conversation file (conversation.sidechains[]); the manifest totals gained
// model_breakdown / op_breakdown maps and each conversation entry gained
// display_id. All additive — a 1.x consumer that ignores unknown fields is
// unaffected.
const SchemaVersion = "dressage.ir/1.2"

// Values of Manifest.RawBodies.
const (
	RawBodiesEmbedded = "embedded" // invocations[].input.json / output.json carry the verbatim payloads
	RawBodiesOmitted  = "omitted"  // payload fields are absent; token/cache accounting is still present
)

// Manifest is the run-level index written to manifest.json. It carries run
// metadata, aggregate totals, and a lightweight entry per conversation so a
// consumer can triage and shard without opening every conversation file.
type Manifest struct {
	SchemaVersion string     `json:"schema_version"`
	GeneratedAt   time.Time  `json:"generated_at"`
	Tool          ToolInfo   `json:"tool"`
	Source        SourceInfo `json:"source"`
	// RawBodies records whether this export embeds verbatim invocation
	// payloads: RawBodiesEmbedded or RawBodiesOmitted. Consumers that need
	// exact wire bodies must check it before relying on invocations[].*.json.
	RawBodies     string          `json:"raw_bodies"`
	Files         ManifestFiles   `json:"files"`
	Totals        ManifestTotals  `json:"totals"`
	Conversations []ManifestEntry `json:"conversations"`
}

// ManifestFiles locates the run-level sibling artifacts within the IR
// directory. Consumers resolve tables through these fields, never by
// hard-coding filenames.
type ManifestFiles struct {
	Facts string `json:"facts,omitempty"` // columnar per-invocation facts table (Parquet)
	Turns string `json:"turns,omitempty"` // columnar deduplicated-turns table (Parquet)
}

// ToolInfo identifies the tool and version that produced the IR.
type ToolInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// SourceInfo carries the provenance of a run: the dominant provider, the command
// line that produced it, and the requested date range. It is supplied by the
// caller (the CLI) and copied verbatim into the manifest.
type SourceInfo struct {
	Provider  string          `json:"provider"`
	Command   string          `json:"command"`
	DateRange ManifestDateRng `json:"date_range"`
}

// ManifestDateRng is the requested reporting window, formatted as YYYY-MM-DD
// dates. Empty strings indicate an unbounded edge.
type ManifestDateRng struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// ManifestTotals holds the report-wide aggregate counters. ModelBreakdown and
// OpBreakdown map a model id / operation name to its invocation count across the
// whole run; both are always non-nil objects (empty for a zero-conversation
// run) so a consumer never has to distinguish {} from null.
type ManifestTotals struct {
	Conversations  int            `json:"conversations"`
	Invocations    int            `json:"invocations"`
	InputTokens    int64          `json:"input_tokens"`
	OutputTokens   int64          `json:"output_tokens"`
	Errors         int            `json:"errors"`
	ModelBreakdown map[string]int `json:"model_breakdown"`
	OpBreakdown    map[string]int `json:"op_breakdown"`
}

// ManifestEntry is one conversation's index entry: enough to triage, shard, and
// locate the full conversation file without opening it.
type ManifestEntry struct {
	ID              string    `json:"id"`
	DisplayID       string    `json:"display_id"` // human-friendly conv-YYYYMMDD-N label (run-order-dependent; not a stable key)
	File            string    `json:"file"`
	Provider        string    `json:"provider"`
	ModelID         string    `json:"model_id"`
	SessionID       string    `json:"session_id,omitempty"`
	StartTime       time.Time `json:"start_time"`
	EndTime         time.Time `json:"end_time"`
	TurnCount       int       `json:"turn_count"`
	InvocationCount int       `json:"invocation_count"`
	InputTokens     int64     `json:"input_tokens"`
	OutputTokens    int64     `json:"output_tokens"`
	ErrorCount      int       `json:"error_count"`
	Reconstructed   bool      `json:"reconstructed"`
}

// ConversationIR is a complete, self-contained conversation IR, written to
// conversations/<id>.json. It carries two layers: the reconstructed Conversation
// view (null for deferred providers) and the raw Invocations (always populated).
type ConversationIR struct {
	SchemaVersion string     `json:"schema_version"`
	ID            string     `json:"id"`
	DisplayID     string     `json:"display_id"` // human-friendly conv-YYYYMMDD-N label (run-order-dependent; not a stable key)
	SessionID     string     `json:"session_id,omitempty"`
	Provider      string     `json:"provider"`
	ModelID       string     `json:"model_id"`
	Identity      IdentityIR `json:"identity"`
	StartTime     time.Time  `json:"start_time"`
	EndTime       time.Time  `json:"end_time"`
	Stats         StatsIR    `json:"stats"`

	// Conversation is the reconstructed main-thread view; nil (JSON null) when no
	// ConversationDetail was reconstructed (e.g. deferred providers).
	Conversation *ConversationView `json:"conversation"`

	// Sidechains are the reconstructed subagent threads spawned within this
	// conversation, each a full ConversationView of its own. Omitted (absent)
	// when the conversation has none — the common single-thread case. The same
	// turns also appear in turns.parquet (thread_id != ""); this field lets a
	// conversation page render subagents without a Parquet join.
	Sidechains []SidechainIR `json:"sidechains,omitempty"`

	// Invocations is the ground-truth layer: every request/response pair with
	// raw provider bodies embedded inline. Always populated.
	Invocations []InvocationIR `json:"invocations"`
}

// SidechainIR is one reconstructed subagent thread: its id (the fetcher-assigned
// thread id that groups the chain) plus a full reconstructed view. Conversation
// is non-nil (a sidechain that failed to reconstruct is dropped upstream).
type SidechainIR struct {
	ID           string            `json:"id"`
	Conversation *ConversationView `json:"conversation"`
}

// IdentityIR is the principal that made the invocations, normalized across
// providers.
type IdentityIR struct {
	Principal string            `json:"principal,omitempty"`
	Display   string            `json:"display,omitempty"`
	Extra     map[string]string `json:"extra,omitempty"`
}

// StatsIR holds the per-conversation aggregate counters.
type StatsIR struct {
	InvocationCount  int   `json:"invocation_count"`
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	ErrorCount       int   `json:"error_count"`
}

// ConversationView is the reconstructed, normalized view of a conversation: the
// system prompt, the available tools, and the sequence of turns. This is what a
// judge reads to understand "what happened."
type ConversationView struct {
	SystemPrompt string   `json:"system_prompt"`
	Tools        []ToolIR `json:"tools"`
	Turns        []TurnIR `json:"turns"`
}

// ToolIR describes a tool available in the conversation. Description and
// InputSchema are full and untruncated.
type ToolIR struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// TurnIR is one message in the reconstructed conversation.
type TurnIR struct {
	Role    string     `json:"role"`
	Blocks  []BlockIR  `json:"blocks"`
	Metrics *MetricsIR `json:"metrics,omitempty"`
}

// BlockIR is one typed content block within a turn. Field presence depends on
// Type (text, thinking, tool_use, tool_result, media); see docs/ir-format.md.
// The type set is open: unrecognized provider types pass through verbatim.
type BlockIR struct {
	Type string `json:"type"`

	// text / thinking
	Text string `json:"text,omitempty"`

	// tool_use
	ToolID    string          `json:"tool_id,omitempty"`
	ToolName  string          `json:"tool_name,omitempty"`
	ToolInput json.RawMessage `json:"tool_input,omitempty"`

	// tool_result
	IsError       bool   `json:"is_error,omitempty"`
	ResultContent string `json:"result_content,omitempty"`

	// media
	MimeType string `json:"mime_type,omitempty"`
	FileURI  string `json:"file_uri,omitempty"`
	Inline   bool   `json:"inline,omitempty"`
	ByteSize int64  `json:"byte_size,omitempty"`
}

// MetricsIR is the per-invocation performance data for an assistant turn.
type MetricsIR struct {
	Timestamp        time.Time `json:"timestamp"`
	RequestID        string    `json:"request_id,omitempty"`
	ModelID          string    `json:"model_id,omitempty"`
	InputTokens      int64     `json:"input_tokens"`
	OutputTokens     int64     `json:"output_tokens"`
	CacheReadTokens  int64     `json:"cache_read_tokens"`
	CacheWriteTokens int64     `json:"cache_write_tokens"`
	LatencyMs        int64     `json:"latency_ms"`
	FirstByteMs      int64     `json:"first_byte_ms"`
	StopReason       string    `json:"stop_reason,omitempty"`
}

// InvocationIR is a single request/response pair as a normalized record,
// including the raw provider JSON bodies embedded inline. This is ground truth:
// an extractor that needs the exact wire payload reads here.
type InvocationIR struct {
	Timestamp      time.Time       `json:"timestamp"`
	RequestID      string          `json:"request_id,omitempty"`
	ModelID        string          `json:"model_id"`
	Operation      string          `json:"operation,omitempty"`
	Status         string          `json:"status,omitempty"`
	ErrorCode      string          `json:"error_code,omitempty"`
	Identity       IdentityIR      `json:"identity"`
	LatencyMs      int64           `json:"latency_ms"`
	Input          BodyIR          `json:"input"`
	Output         BodyIR          `json:"output"`
	ProviderExtras json.RawMessage `json:"provider_extras,omitempty"`
}

// BodyIR is a request or response payload plus its token accounting. The raw
// JSON is embedded inline and verbatim.
type BodyIR struct {
	ContentType string          `json:"content_type,omitempty"`
	TokenCount  int64           `json:"token_count"`
	CacheRead   int64           `json:"cache_read"`
	CacheWrite  int64           `json:"cache_write"`
	JSON        json.RawMessage `json:"json,omitempty"`
}
