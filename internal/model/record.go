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
	Identity       Identity
	Input          Body
	Output         Body
	LatencyMs      int64           // request latency where the provider reports it at the record level (0 if unknown)
	ProviderExtras json.RawMessage // opaque per-provider fields, not rendered yet
}

// Identity is the principal that made the invocation, normalized across providers.
type Identity struct {
	Principal string            // ARN (Bedrock), Entra OID (Azure), SA email (Vertex); may be empty when the source lacks per-record identity
	Display   string            // human-friendly label for the report; may be empty
	Extra     map[string]string // provider-specific attributes: accountId, region, subscription, project, ...
}

// Body holds a request or response payload plus its token accounting.
type Body struct {
	JSON        json.RawMessage
	ContentType string
	TokenCount  int64
	CacheRead   int64 // cache-read input tokens (0 if provider does not report)
	CacheWrite  int64 // cache-write/creation input tokens (0 if provider does not report; e.g. always 0 for OpenAI/Gemini)
}
