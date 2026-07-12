// Package model defines provider-neutral data types for LLM invocation logs.
package model

import (
	"encoding/json"
	"time"
)

// Record is a provider-neutral normalized invocation log entry. Every provider
// fetcher decodes its own on-the-wire format and emits []Record.
type Record struct {
	Provider       string // "bedrock", "azure", "vertex", ...
	Timestamp      time.Time
	RequestID      string
	ModelID        string // canonical model/deployment id
	Operation      string // provider operation name, e.g. "InvokeModelWithResponseStream", "ChatCompletions_Create"
	Status         string // raw provider status string, e.g. "200" (passed through to the report unchanged)
	ErrorCode      string // non-empty marks an errored invocation
	StopReason     string // response stop/finish reason when the fetcher lifts it cheaply from the response envelope; "" otherwise
	Identity       Identity
	SessionID      string // session id pre-extracted from the request body; "" when absent or when the fetcher leaves extraction to the summary layer
	Correlation    Correlation
	Input          Body
	Output         Body
	LatencyMs      int64           // request latency where the provider reports it at the record level (0 if unknown)
	ProviderExtras json.RawMessage // opaque per-provider fields, not rendered yet
}

// Correlation carries provider-assigned message linkage for one invocation,
// populated by fetchers whose capture format exposes it (currently the local
// "claude" raw-body capture). All fields are zero for providers without
// linkage data.
type Correlation struct {
	MessageID     string // response message id (e.g. "msg_...")
	PrevMessageID string // prior turn's response message id named by the request
	ThreadID      string // stable id of the request chain this invocation belongs to
	NumMessages   int    // number of messages in the request transcript (0 if unknown)
}

// Identity is the principal that made the invocation, normalized across providers.
type Identity struct {
	Principal string            // ARN (Bedrock), Entra OID (Azure), SA email (Vertex); may be empty when the source lacks per-record identity
	Display   string            // human-friendly label for the report; may be empty
	Extra     map[string]string // provider-specific attributes: accountId, region, subscription, project, ...
}

// BodySource supplies a payload on demand. Fetchers whose payloads live
// outside the record (e.g. on disk) provide one so records stay small; callers
// obtain bytes via Body.Load and must not retain the result beyond the
// conversation being processed.
type BodySource interface {
	Load() (json.RawMessage, error)
}

// Body holds a request or response payload plus its token accounting. The
// payload is either inline (JSON) or lazy (Source); at most one is set.
type Body struct {
	JSON        json.RawMessage // inline payload; nil when Source is set
	Source      BodySource      // lazy payload; nil when the body is inline or absent
	ContentType string
	TokenCount  int64
	CacheRead   int64 // cache-read input tokens (0 if provider does not report)
	CacheWrite  int64 // cache-write/creation input tokens (0 if provider does not report; e.g. always 0 for OpenAI/Gemini)
}

// Load returns the body payload, reading it from Source when the body is lazy.
func (b Body) Load() (json.RawMessage, error) {
	if b.Source != nil {
		return b.Source.Load()
	}
	return b.JSON, nil
}

// Present reports whether the body carries a payload, inline or lazy.
func (b Body) Present() bool {
	return len(b.JSON) > 0 || b.Source != nil
}
