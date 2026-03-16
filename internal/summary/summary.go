// Package summary groups and summarizes Bedrock model invocation logs
// into a structured report organized by day and conversation.
package summary

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/rubynerd/dressage/internal/conversation"
	"github.com/rubynerd/dressage/internal/model"
)

// conversationGap is the maximum duration between consecutive invocations
// within the same (modelId, identityARN) group before they are split into
// separate conversations.
const conversationGap = 5 * time.Minute

// Summarize takes a slice of invocation logs and produces a complete Report
// grouped by day and conversation.
func Summarize(logs []model.InvocationLog) *model.Report {
	now := time.Now().UTC()

	if len(logs) == 0 {
		return &model.Report{
			GeneratedAt: now,
			TotalStats:  emptyStats(),
		}
	}

	// Sort all logs by timestamp.
	sorted := make([]model.InvocationLog, len(logs))
	copy(sorted, logs)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Timestamp.Before(sorted[j].Timestamp)
	})

	// Group logs by UTC date.
	dayBuckets := groupByDay(sorted)

	// Build per-day summaries.
	var days []model.DaySummary
	totalStats := emptyStats()
	globalConvIndex := 0

	// Collect day keys and sort them.
	dayKeys := make([]string, 0, len(dayBuckets))
	for k := range dayBuckets {
		dayKeys = append(dayKeys, k)
	}
	sort.Strings(dayKeys)

	for _, dayKey := range dayKeys {
		dayLogs := dayBuckets[dayKey]
		dayDate := dayLogs[0].Timestamp.UTC().Truncate(24 * time.Hour)

		conversations, nextIndex := buildConversations(dayLogs, dayKey, globalConvIndex)
		globalConvIndex = nextIndex

		dayStats := computeStats(dayLogs)
		mergeStats(&totalStats, &dayStats)

		days = append(days, model.DaySummary{
			Date:          dayDate,
			Stats:         dayStats,
			Conversations: conversations,
		})
	}

	dateRange := model.DateRange{
		Start: sorted[0].Timestamp.UTC(),
		End:   sorted[len(sorted)-1].Timestamp.UTC(),
	}

	return &model.Report{
		GeneratedAt: now,
		DateRange:   dateRange,
		TotalStats:  totalStats,
		Days:        days,
	}
}

// groupByDay buckets logs by their UTC date string (YYYYMMDD).
func groupByDay(logs []model.InvocationLog) map[string][]model.InvocationLog {
	buckets := make(map[string][]model.InvocationLog)
	for _, log := range logs {
		key := log.Timestamp.UTC().Format("20060102")
		buckets[key] = append(buckets[key], log)
	}
	return buckets
}

// groupKey identifies a unique (modelId, identityARN) pair for conversation grouping.
type groupKey struct {
	ModelID     string
	IdentityARN string
}

// buildConversations groups a day's logs into conversations.
// It first attempts session-based grouping using the session ID extracted from
// inputBodyJson metadata (used by Claude Code). Logs without session IDs fall
// back to the (modelId, identityARN) + 5-minute gap heuristic.
func buildConversations(dayLogs []model.InvocationLog, dayKey string, startIndex int) ([]model.ConversationSummary, int) {
	// Partition logs: those with session IDs vs those without.
	sessionGroups := make(map[string][]model.InvocationLog)
	var noSessionLogs []model.InvocationLog

	for _, lg := range dayLogs {
		sid := conversation.ExtractSessionID(lg.Input.InputBodyJSON)
		if sid != "" {
			sessionGroups[sid] = append(sessionGroups[sid], lg)
		} else {
			noSessionLogs = append(noSessionLogs, lg)
		}
	}

	convIndex := startIndex
	var conversations []model.ConversationSummary

	// Process session-based groups.
	sessionIDs := make([]string, 0, len(sessionGroups))
	for sid := range sessionGroups {
		sessionIDs = append(sessionIDs, sid)
	}
	sort.Strings(sessionIDs)

	for _, sid := range sessionIDs {
		groupLogs := sessionGroups[sid]
		sort.Slice(groupLogs, func(i, j int) bool {
			return groupLogs[i].Timestamp.Before(groupLogs[j].Timestamp)
		})

		convID := fmt.Sprintf("conv-%s-%d", dayKey, convIndex)
		convIndex++
		cs := buildConversationSummary(convID, groupLogs)
		cs.SessionID = sid

		// Attempt full conversation reconstruction.
		detail := conversation.Reconstruct(groupLogs)
		if detail != nil {
			cs.Detail = detail
			log.Printf("Reconstructed conversation %s: %d turns, session %s",
				convID, len(detail.Turns), sid[:8])
		}
		conversations = append(conversations, cs)
	}

	// Process remaining logs without session IDs using the time-gap heuristic.
	if len(noSessionLogs) > 0 {
		gapConvs, nextIdx := buildConversationsTimeBased(noSessionLogs, dayKey, convIndex)
		conversations = append(conversations, gapConvs...)
		convIndex = nextIdx
	}

	// Sort conversations by start time for consistent display.
	sort.Slice(conversations, func(i, j int) bool {
		return conversations[i].StartTime.Before(conversations[j].StartTime)
	})

	return conversations, convIndex
}

// buildConversationsTimeBased groups logs using the (modelId, identityARN) +
// 5-minute gap heuristic. This is the fallback for logs without session IDs.
func buildConversationsTimeBased(dayLogs []model.InvocationLog, dayKey string, startIndex int) ([]model.ConversationSummary, int) {
	groups := make(map[groupKey][]model.InvocationLog)
	for _, lg := range dayLogs {
		k := groupKey{ModelID: lg.ModelID, IdentityARN: lg.Identity.ARN}
		groups[k] = append(groups[k], lg)
	}

	keys := make([]groupKey, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].ModelID != keys[j].ModelID {
			return keys[i].ModelID < keys[j].ModelID
		}
		return keys[i].IdentityARN < keys[j].IdentityARN
	})

	convIndex := startIndex
	var conversations []model.ConversationSummary

	for _, k := range keys {
		groupLogs := groups[k]
		sort.Slice(groupLogs, func(i, j int) bool {
			return groupLogs[i].Timestamp.Before(groupLogs[j].Timestamp)
		})

		var conv []model.InvocationLog
		for i, lg := range groupLogs {
			if i > 0 && lg.Timestamp.Sub(groupLogs[i-1].Timestamp) > conversationGap {
				convID := fmt.Sprintf("conv-%s-%d", dayKey, convIndex)
				convIndex++
				conversations = append(conversations, buildConversationSummary(convID, conv))
				conv = nil
			}
			conv = append(conv, lg)
		}
		if len(conv) > 0 {
			convID := fmt.Sprintf("conv-%s-%d", dayKey, convIndex)
			convIndex++
			conversations = append(conversations, buildConversationSummary(convID, conv))
		}
	}

	return conversations, convIndex
}

// buildConversationSummary creates a ConversationSummary from a slice of
// chronologically ordered invocation logs belonging to one conversation.
func buildConversationSummary(id string, logs []model.InvocationLog) model.ConversationSummary {
	summary := model.ConversationSummary{
		ID:          id,
		ModelID:     logs[0].ModelID,
		IdentityARN: logs[0].Identity.ARN,
		StartTime:   logs[0].Timestamp,
		EndTime:     logs[len(logs)-1].Timestamp,
		MessageCount: len(logs),
	}

	invocations := make([]model.Invocation, 0, len(logs))
	for _, log := range logs {
		summary.InputTokens += log.Input.InputTokenCount
		summary.OutputTokens += log.Output.OutputTokenCount
		if log.ErrorCode != "" {
			summary.ErrorCount++
		}

		invocations = append(invocations, model.Invocation{
			Timestamp:    log.Timestamp,
			RequestID:    log.RequestID,
			ModelID:      log.ModelID,
			Operation:    log.Operation,
			Status:       log.Status,
			ErrorCode:    log.ErrorCode,
			InputBody:    prettyJSON(log.Input.InputBodyJSON),
			OutputBody:   prettyJSON(log.Output.OutputBodyJSON),
			InputTokens:  log.Input.InputTokenCount,
			OutputTokens: log.Output.OutputTokenCount,
			IdentityARN:  log.Identity.ARN,
		})
	}
	summary.Invocations = invocations
	return summary
}

// computeStats calculates aggregate statistics for a set of logs.
func computeStats(logs []model.InvocationLog) model.Stats {
	s := emptyStats()
	s.InvocationCount = len(logs)
	for _, log := range logs {
		s.InputTokens += log.Input.InputTokenCount
		s.OutputTokens += log.Output.OutputTokenCount
		if log.ErrorCode != "" {
			s.ErrorCount++
		}
		s.ModelBreakdown[log.ModelID]++
		s.OpBreakdown[log.Operation]++
	}
	return s
}

// mergeStats adds the values from src into dst.
func mergeStats(dst, src *model.Stats) {
	dst.InvocationCount += src.InvocationCount
	dst.InputTokens += src.InputTokens
	dst.OutputTokens += src.OutputTokens
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

// prettyJSON attempts to pretty-print a JSON raw message.
// If the input is nil, empty, or invalid JSON, it returns the raw string as-is.
func prettyJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	pretty, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return string(raw)
	}
	return string(pretty)
}
