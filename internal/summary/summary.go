// Package summary groups and summarizes normalized invocation records
// into a structured report organized by day and conversation.
package summary

import (
	"fmt"
	"iter"
	"log"
	"sort"
	"time"

	"github.com/rxbynerd/dressage/internal/conversation"
	"github.com/rxbynerd/dressage/internal/model"
)

// conversationGap is the maximum duration between consecutive invocations
// within the same (provider, modelId, principal) group before they are split
// into separate conversations.
const conversationGap = 5 * time.Minute

// Summarize takes a slice of normalized invocation records and produces a
// complete Report grouped by day and conversation, with every conversation
// materialized (reconstruction attached) and retained. Callers that stream
// conversations out one at a time — and so never need the whole materialized
// report in memory — use NewPlan directly instead. Retained here because the
// IR golden tests and downstream callers want the full report in hand.
func Summarize(records []model.Record) *model.Report {
	p := NewPlan(records)
	for range p.Conversations(MaterializeOptions{Retain: true}) {
	}
	return p.Report()
}

// Plan is the metadata-only grouping of records into days and conversations.
// Building one performs no body loads: session ids come from Record.SessionID
// (or, for inline bodies only, extraction from the raw request), and all stats
// derive from record-level token accounting. Conversations are materialized —
// reconstruction, rendered bodies — one at a time via Conversations.
type Plan struct {
	report *model.Report
	convs  []convPlan
}

// convPlan is one planned conversation: its identity plus the records that
// make it up, in day-major display order.
type convPlan struct {
	dayIdx    int
	id        string
	sessionID string // "" for gap-grouped conversations
	records   []model.Record
}

// MaterializeOptions controls how Plan.Conversations materializes each
// conversation.
type MaterializeOptions struct {
	// Retain keeps each materialized conversation in the Plan's report (the
	// Summarize behavior). When false a conversation is dropped after its yield
	// returns, bounding memory to one conversation at a time.
	Retain bool
}

// NewPlan groups records into days and conversations without materializing
// any conversation content.
func NewPlan(records []model.Record) *Plan {
	now := time.Now().UTC()
	p := &Plan{report: &model.Report{
		GeneratedAt: now,
		TotalStats:  emptyStats(),
	}}

	if len(records) == 0 {
		return p
	}

	// Sort all records by timestamp.
	sorted := make([]model.Record, len(records))
	copy(sorted, records)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Timestamp.Before(sorted[j].Timestamp)
	})

	// Group records by UTC date.
	dayBuckets := groupByDay(sorted)

	// Collect day keys and sort them.
	dayKeys := make([]string, 0, len(dayBuckets))
	for k := range dayBuckets {
		dayKeys = append(dayKeys, k)
	}
	sort.Strings(dayKeys)

	globalConvIndex := 0
	for dayIdx, dayKey := range dayKeys {
		dayRecords := dayBuckets[dayKey]
		dayDate := dayRecords[0].Timestamp.UTC().Truncate(24 * time.Hour)

		convs, nextIndex := planConversations(dayRecords, dayKey, globalConvIndex)
		globalConvIndex = nextIndex
		for i := range convs {
			convs[i].dayIdx = dayIdx
		}
		p.convs = append(p.convs, convs...)

		dayStats := computeStats(dayRecords)
		mergeStats(&p.report.TotalStats, &dayStats)

		p.report.Days = append(p.report.Days, model.DaySummary{
			Date:  dayDate,
			Stats: dayStats,
		})
	}

	p.report.DateRange = model.DateRange{
		Start: sorted[0].Timestamp.UTC(),
		End:   sorted[len(sorted)-1].Timestamp.UTC(),
	}
	return p
}

// Report returns the plan's report. Before any Conversations drain it is a
// skeleton — dates, stats and totals populated, per-day Conversations empty;
// after a drain with Retain set it is the complete Summarize output.
func (p *Plan) Report() *model.Report {
	return p.report
}

// Conversations materializes the planned conversations one at a time, in the
// report's display order (day-major, then start time). Session-grouped
// conversations get full reconstruction attached; gap-grouped ones do not
// (matching Summarize's historical behavior). The yielded pointer is only
// valid for the duration of the yield unless opts.Retain is set.
func (p *Plan) Conversations(opts MaterializeOptions) iter.Seq[*model.ConversationSummary] {
	return func(yield func(*model.ConversationSummary) bool) {
		for i := range p.convs {
			cp := &p.convs[i]
			cs := buildConversationSummary(cp.id, cp.records)
			if cp.sessionID != "" {
				cs.SessionID = cp.sessionID
				if detail, sidechains := conversation.ReconstructThreads(cp.records); detail != nil {
					cs.Detail = detail
					cs.Sidechains = sidechains
					log.Printf("Reconstructed conversation %s: %d turns, %d sidechains, session %s",
						cp.id, len(detail.Turns), len(sidechains), shortID(cp.sessionID))
				}
			}
			if opts.Retain {
				day := &p.report.Days[cp.dayIdx]
				day.Conversations = append(day.Conversations, cs)
				if !yield(&day.Conversations[len(day.Conversations)-1]) {
					return
				}
			} else if !yield(&cs) {
				return
			}
		}
	}
}

// groupByDay buckets records by their UTC date string (YYYYMMDD).
func groupByDay(records []model.Record) map[string][]model.Record {
	buckets := make(map[string][]model.Record)
	for _, rec := range records {
		key := rec.Timestamp.UTC().Format("20060102")
		buckets[key] = append(buckets[key], rec)
	}
	return buckets
}

// groupKey identifies a unique (provider, modelId, principal) tuple for
// conversation grouping.
type groupKey struct {
	Provider  string
	ModelID   string
	Principal string
}

// sessionKey identifies a session-grouped conversation. The provider is part of
// the key because session ids are extracted per-provider and may collide across
// providers (e.g. a substring shared between a Bedrock and an Azure session id);
// keying on the session id alone would merge unrelated records and then decode
// them with the wrong envelope in Reconstruct.
type sessionKey struct {
	Provider string
	SID      string
}

// shortID returns at most the first 8 characters of s, used for log lines. It
// is a safe replacement for s[:8], which panics on shorter strings.
func shortID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// planConversations groups a day's records into planned conversations.
// It first attempts session-based grouping using the session ID extracted from
// the request body (used by Claude Code). Records without session IDs fall
// back to the (provider, modelId, principal) + 5-minute gap heuristic.
// Conversation ids are assigned in grouping order (session groups first, then
// gap groups) and the result is then sorted into display order by start time,
// matching the historical Summarize output exactly.
func planConversations(dayRecords []model.Record, dayKey string, startIndex int) ([]convPlan, int) {
	// Partition records: those with session IDs vs those without. Session groups
	// are keyed on (provider, session id) so that records from different
	// providers are never merged even when their session ids collide.
	sessionGroups := make(map[sessionKey][]model.Record)
	var noSessionRecords []model.Record

	for _, rec := range dayRecords {
		sid := sessionID(rec)
		if sid != "" {
			key := sessionKey{Provider: rec.Provider, SID: sid}
			sessionGroups[key] = append(sessionGroups[key], rec)
		} else {
			noSessionRecords = append(noSessionRecords, rec)
		}
	}

	convIndex := startIndex
	var convs []convPlan

	// Process session-based groups, ordered by provider then session id for
	// deterministic output. For single-provider input every provider is equal,
	// so this collapses to session-id order.
	sessionKeys := make([]sessionKey, 0, len(sessionGroups))
	for key := range sessionGroups {
		sessionKeys = append(sessionKeys, key)
	}
	sort.Slice(sessionKeys, func(i, j int) bool {
		if sessionKeys[i].Provider != sessionKeys[j].Provider {
			return sessionKeys[i].Provider < sessionKeys[j].Provider
		}
		return sessionKeys[i].SID < sessionKeys[j].SID
	})

	for _, key := range sessionKeys {
		groupRecords := sessionGroups[key]
		sort.Slice(groupRecords, func(i, j int) bool {
			return groupRecords[i].Timestamp.Before(groupRecords[j].Timestamp)
		})

		convs = append(convs, convPlan{
			id:        fmt.Sprintf("conv-%s-%d", dayKey, convIndex),
			sessionID: key.SID,
			records:   groupRecords,
		})
		convIndex++
	}

	// Process remaining records without session IDs using the time-gap heuristic.
	if len(noSessionRecords) > 0 {
		gapConvs, nextIdx := planConversationsTimeBased(noSessionRecords, dayKey, convIndex)
		convs = append(convs, gapConvs...)
		convIndex = nextIdx
	}

	// Sort conversations by start time for consistent display.
	sort.Slice(convs, func(i, j int) bool {
		return convs[i].records[0].Timestamp.Before(convs[j].records[0].Timestamp)
	})

	return convs, convIndex
}

// sessionID returns a record's session id: the fetcher-provided value when
// present, otherwise extracted from the request body. Records with lazy bodies
// are expected to arrive with SessionID pre-extracted, so grouping never loads
// a lazy body just to read its session id.
func sessionID(rec model.Record) string {
	if rec.SessionID != "" {
		return rec.SessionID
	}
	if rec.Input.Source != nil {
		return ""
	}
	return conversation.ExtractSessionID(rec.Provider, rec.ModelID, rec.Input.JSON)
}

// planConversationsTimeBased groups records using the
// (provider, modelId, principal) + 5-minute gap heuristic. This is the
// fallback for records without session IDs.
func planConversationsTimeBased(dayRecords []model.Record, dayKey string, startIndex int) ([]convPlan, int) {
	groups := make(map[groupKey][]model.Record)
	for _, rec := range dayRecords {
		k := groupKey{Provider: rec.Provider, ModelID: rec.ModelID, Principal: rec.Identity.Principal}
		groups[k] = append(groups[k], rec)
	}

	keys := make([]groupKey, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Provider != keys[j].Provider {
			return keys[i].Provider < keys[j].Provider
		}
		if keys[i].ModelID != keys[j].ModelID {
			return keys[i].ModelID < keys[j].ModelID
		}
		return keys[i].Principal < keys[j].Principal
	})

	convIndex := startIndex
	var convs []convPlan

	appendConv := func(records []model.Record) {
		convs = append(convs, convPlan{
			id:      fmt.Sprintf("conv-%s-%d", dayKey, convIndex),
			records: records,
		})
		convIndex++
	}

	for _, k := range keys {
		groupRecords := groups[k]
		sort.Slice(groupRecords, func(i, j int) bool {
			return groupRecords[i].Timestamp.Before(groupRecords[j].Timestamp)
		})

		var conv []model.Record
		for i, rec := range groupRecords {
			if i > 0 && rec.Timestamp.Sub(groupRecords[i-1].Timestamp) > conversationGap {
				appendConv(conv)
				conv = nil
			}
			conv = append(conv, rec)
		}
		if len(conv) > 0 {
			appendConv(conv)
		}
	}

	return convs, convIndex
}

// buildConversationSummary creates a ConversationSummary from a slice of
// chronologically ordered invocation records belonging to one conversation.
func buildConversationSummary(id string, records []model.Record) model.ConversationSummary {
	summary := model.ConversationSummary{
		ID:           id,
		Provider:     records[0].Provider,
		ModelID:      records[0].ModelID,
		Identity:     records[0].Identity.Principal,
		StartTime:    records[0].Timestamp,
		EndTime:      records[len(records)-1].Timestamp,
		MessageCount: len(records),
	}

	invocations := make([]model.Invocation, 0, len(records))
	for _, rec := range records {
		summary.InputTokens += rec.Input.TokenCount
		summary.OutputTokens += rec.Output.TokenCount
		if rec.ErrorCode != "" {
			summary.ErrorCount++
		}

		// Preserve the raw record verbatim for faithful machine-readable export.
		invocations = append(invocations, model.Invocation{
			Timestamp:      rec.Timestamp,
			RequestID:      rec.RequestID,
			ModelID:        rec.ModelID,
			Operation:      rec.Operation,
			Status:         rec.Status,
			ErrorCode:      rec.ErrorCode,
			LatencyMs:      rec.LatencyMs,
			StopReason:     rec.StopReason,
			Correlation:    rec.Correlation,
			FullIdentity:   rec.Identity,
			Input:          rec.Input,
			Output:         rec.Output,
			ProviderExtras: rec.ProviderExtras,
		})
	}
	summary.Invocations = invocations
	return summary
}

// computeStats calculates aggregate statistics for a set of records.
func computeStats(records []model.Record) model.Stats {
	s := emptyStats()
	s.InvocationCount = len(records)
	for _, rec := range records {
		s.InputTokens += rec.Input.TokenCount
		s.OutputTokens += rec.Output.TokenCount
		s.CacheReadTokens += rec.Input.CacheRead
		s.CacheWriteTokens += rec.Input.CacheWrite
		if rec.ErrorCode != "" {
			s.ErrorCount++
		}
		s.ModelBreakdown[rec.ModelID]++
		s.OpBreakdown[rec.Operation]++
	}
	return s
}

// mergeStats adds the values from src into dst.
func mergeStats(dst, src *model.Stats) {
	dst.InvocationCount += src.InvocationCount
	dst.InputTokens += src.InputTokens
	dst.OutputTokens += src.OutputTokens
	dst.CacheReadTokens += src.CacheReadTokens
	dst.CacheWriteTokens += src.CacheWriteTokens
	dst.ErrorCount += src.ErrorCount
	for k, v := range src.ModelBreakdown {
		dst.ModelBreakdown[k] += v
	}
	for k, v := range src.OpBreakdown {
		dst.OpBreakdown[k] += v
	}
}

// emptyStats returns a Stats value with initialized maps.
func emptyStats() model.Stats {
	return model.Stats{
		ModelBreakdown: make(map[string]int),
		OpBreakdown:    make(map[string]int),
	}
}
