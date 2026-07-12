package ir

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/rxbynerd/dressage/internal/model"
)

// TurnsFile is the deduplicated-turns table's filename within an IR directory.
const TurnsFile = "turns.parquet"

// TurnRow is one deduplicated conversation turn in the columnar turns table
// (turns.parquet): the content layer a downstream engine scans or full-text
// indexes without opening per-conversation JSON files. Rows come from the
// reconstruction, so each unique turn appears once regardless of how many
// times a resend-style provider re-transmitted it.
//
// conversation_id joins to the manifest (and facts table). thread_id is ""
// for the main thread and the sidechain's id for subagent threads. text
// flattens the turn's text blocks (the FTS target); blocks carries the full
// typed content as a JSON array in the BlockIR shape. Metrics columns are the
// turn's TurnMetrics where present; has_metrics distinguishes a real
// zero-valued metric from an absent one.
type TurnRow struct {
	ConversationID string `parquet:"conversation_id"`
	SessionID      string `parquet:"session_id"`
	Provider       string `parquet:"provider"`
	ThreadID       string `parquet:"thread_id"`
	TurnIndex      int32  `parquet:"turn_index"`
	Role           string `parquet:"role"`
	Text           string `parquet:"text"`
	Blocks         string `parquet:"blocks"`

	HasMetrics       bool      `parquet:"has_metrics"`
	Timestamp        time.Time `parquet:"timestamp"`
	RequestID        string    `parquet:"request_id"`
	ModelID          string    `parquet:"model_id"`
	InputTokens      int64     `parquet:"input_tokens"`
	OutputTokens     int64     `parquet:"output_tokens"`
	CacheReadTokens  int64     `parquet:"cache_read_tokens"`
	CacheWriteTokens int64     `parquet:"cache_write_tokens"`
	LatencyMs        int64     `parquet:"latency_ms"`
	FirstByteMs      int64     `parquet:"first_byte_ms"`
	StopReason       string    `parquet:"stop_reason"`
}

// mapTurns translates one conversation's reconstructed content — main thread
// plus sidechains — into turns rows. Conversations without a reconstruction
// yield no rows. Pure.
func mapTurns(cs model.ConversationSummary) []TurnRow {
	if cs.Detail == nil {
		return nil
	}
	convID := stableID(cs)
	rows := make([]TurnRow, 0, len(cs.Detail.Turns))
	for i, turn := range cs.Detail.Turns {
		rows = append(rows, turnRow(cs, convID, "", int32(i), turn))
	}
	for _, sc := range cs.Sidechains {
		if sc.Detail == nil {
			continue
		}
		for i, turn := range sc.Detail.Turns {
			rows = append(rows, turnRow(cs, convID, sc.ID, int32(i), turn))
		}
	}
	return rows
}

// turnRow builds one row from a reconstructed turn, flattening text blocks
// into the searchable text column and serializing the full block list (BlockIR
// shape) into the blocks column.
func turnRow(cs model.ConversationSummary, convID, threadID string, index int32, turn model.Turn) TurnRow {
	row := TurnRow{
		ConversationID: convID,
		SessionID:      cs.SessionID,
		Provider:       cs.Provider,
		ThreadID:       threadID,
		TurnIndex:      index,
		Role:           turn.Role,
		Text:           turnText(turn),
		Blocks:         blocksJSON(turn),
	}
	if m := turn.Metrics; m != nil {
		row.HasMetrics = true
		row.Timestamp = m.Timestamp
		row.RequestID = m.RequestID
		row.ModelID = m.ModelID
		row.InputTokens = m.InputTokens
		row.OutputTokens = m.OutputTokens
		row.CacheReadTokens = m.CacheReadTokens
		row.CacheWriteTokens = m.CacheWriteTokens
		row.LatencyMs = m.LatencyMs
		row.FirstByteMs = m.FirstByteMs
		row.StopReason = m.StopReason
	}
	return row
}

// turnText flattens a turn's text blocks into one searchable string.
func turnText(turn model.Turn) string {
	var parts []string
	for _, b := range turn.Blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// blocksJSON serializes a turn's blocks as a JSON array in the BlockIR shape,
// so the column matches the per-conversation JSON files field-for-field.
func blocksJSON(turn model.Turn) string {
	blocks := make([]BlockIR, 0, len(turn.Blocks))
	for _, b := range turn.Blocks {
		blocks = append(blocks, mapBlock(b))
	}
	data, err := json.Marshal(blocks)
	if err != nil {
		return "[]"
	}
	return string(data)
}
