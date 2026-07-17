package ir

import (
	"encoding/json"
	"time"

	"github.com/rxbynerd/dressage/internal/model"
)

// FactsFile is the facts table's filename within an IR directory.
const FactsFile = "facts.parquet"

// FactRow is one invocation in the columnar facts table (facts.parquet): the
// per-invocation metadata a downstream engine (e.g. DuckDB) scans without
// touching conversation content. One row per invocation, including errored and
// sidechain invocations. Absent values are the zero value ("" / 0) — the
// schema keeps every column required so output stays deterministic and
// consumers filter with `<> ”` / `> 0` rather than IS NOT NULL.
//
// conversation_id joins to the manifest's conversations[].id (and so to the
// per-conversation JSON file and the turns table). thread_id groups the
// invocations of one linear chain within a session; message_id /
// prev_message_id / request_uuid / num_messages carry provider correlation
// where the fetcher lifted it (currently the claude provider).
type FactRow struct {
	ConversationID   string    `parquet:"conversation_id"`
	SessionID        string    `parquet:"session_id"`
	Provider         string    `parquet:"provider"`
	ModelID          string    `parquet:"model_id"`
	Timestamp        time.Time `parquet:"timestamp"`
	RequestID        string    `parquet:"request_id"`
	Operation        string    `parquet:"operation"`
	Status           string    `parquet:"status"`
	ErrorCode        string    `parquet:"error_code"`
	StopReason       string    `parquet:"stop_reason"`
	InputTokens      int64     `parquet:"input_tokens"`
	OutputTokens     int64     `parquet:"output_tokens"`
	CacheReadTokens  int64     `parquet:"cache_read_tokens"`
	CacheWriteTokens int64     `parquet:"cache_write_tokens"`
	LatencyMs        int64     `parquet:"latency_ms"`
	Principal        string    `parquet:"principal"`
	MessageID        string    `parquet:"message_id"`
	PrevMessageID    string    `parquet:"prev_message_id"`
	ThreadID         string    `parquet:"thread_id"`
	RequestUUID      string    `parquet:"request_uuid"`
	NumMessages      int32     `parquet:"num_messages"`
	Extras           string    `parquet:"extras"` // JSON object of provider-specific identity attributes; "" when none
}

// mapFacts translates one conversation's invocations into facts rows. Pure —
// it reads only accounting/metadata fields, never body payloads.
func mapFacts(cs model.ConversationSummary) []FactRow {
	convID := stableID(cs)
	rows := make([]FactRow, 0, len(cs.Invocations))
	for _, inv := range cs.Invocations {
		rows = append(rows, FactRow{
			ConversationID:   convID,
			SessionID:        cs.SessionID,
			Provider:         cs.Provider,
			ModelID:          inv.ModelID,
			Timestamp:        inv.Timestamp,
			RequestID:        inv.RequestID,
			Operation:        inv.Operation,
			Status:           inv.Status,
			ErrorCode:        inv.ErrorCode,
			StopReason:       inv.StopReason,
			InputTokens:      inv.Input.TokenCount,
			OutputTokens:     inv.Output.TokenCount,
			CacheReadTokens:  inv.Input.CacheRead,
			CacheWriteTokens: inv.Input.CacheWrite,
			LatencyMs:        inv.LatencyMs,
			Principal:        inv.FullIdentity.Principal,
			MessageID:        inv.Correlation.MessageID,
			PrevMessageID:    inv.Correlation.PrevMessageID,
			ThreadID:         inv.Correlation.ThreadID,
			RequestUUID:      inv.Correlation.RequestUUID,
			NumMessages:      int32(inv.Correlation.NumMessages),
			Extras:           identityExtras(inv.FullIdentity),
		})
	}
	return rows
}

// identityExtras flattens provider-specific identity attributes into a
// deterministic JSON object string (map marshaling sorts keys). session_id is
// excluded — it has a first-class column. Returns "" when nothing remains.
func identityExtras(id model.Identity) string {
	if len(id.Extra) == 0 {
		return ""
	}
	extras := make(map[string]string, len(id.Extra))
	for k, v := range id.Extra {
		if k == "session_id" {
			continue
		}
		extras[k] = v
	}
	if len(extras) == 0 {
		return ""
	}
	b, err := json.Marshal(extras)
	if err != nil {
		return ""
	}
	return string(b)
}
