package ir

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/rxbynerd/dressage/internal/model"
)

// toolName identifies the producing tool in the manifest.
const toolName = "dressage"

// Version is the tool version stamped into the manifest's tool block. The CLI
// sets it from its build-time -ldflags version; it defaults to "dev" so the
// package is usable (and testable) without that wiring.
var Version = "dev"

// Export writes the IR for a report to dir: one JSON file per conversation under
// dir/conversations/, plus a manifest.json index at dir's root. It mirrors
// report.Generate's role — it takes the same *model.Report and a destination,
// and is the only IO-touching function in this package; all model.* -> ir.*
// translation stays pure in map.go.
//
// Output is deterministic: manifest.conversations is sorted by start time then
// id, every file is indented JSON, and file names derive from the stable id, so
// two runs over the same report produce byte-identical output (enabling golden
// tests and clean diffs).
func Export(report *model.Report, dir string, src SourceInfo) error {
	if report == nil {
		return fmt.Errorf("ir export: nil report")
	}

	convDir := filepath.Join(dir, "conversations")
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		return fmt.Errorf("creating IR directory %s: %w", convDir, err)
	}

	manifest := Manifest{
		SchemaVersion: SchemaVersion,
		GeneratedAt:   report.GeneratedAt,
		Tool:          ToolInfo{Name: toolName, Version: Version},
		Source:        src,
		Totals:        mapTotals(report),
	}

	// Walk every conversation across every day, mapping and writing each.
	for _, day := range report.Days {
		for _, cs := range day.Conversations {
			conv := mapConversation(cs)
			if err := writeJSON(filepath.Join(dir, filepath.FromSlash(conversationFile(conv.ID))), conv); err != nil {
				return fmt.Errorf("writing conversation %s: %w", conv.ID, err)
			}
			manifest.Conversations = append(manifest.Conversations, mapEntry(conv))
		}
	}

	// Sort the index for byte-stable output: by start time, then id.
	sort.Slice(manifest.Conversations, func(i, j int) bool {
		a, b := manifest.Conversations[i], manifest.Conversations[j]
		if !a.StartTime.Equal(b.StartTime) {
			return a.StartTime.Before(b.StartTime)
		}
		return a.ID < b.ID
	})

	if err := writeJSON(filepath.Join(dir, "manifest.json"), manifest); err != nil {
		return fmt.Errorf("writing manifest: %w", err)
	}
	return nil
}

// ConversationCount returns the number of conversations a report would export.
// The CLI uses it to report the conversation-file count without re-walking the
// IR on disk.
func ConversationCount(report *model.Report) int {
	if report == nil {
		return 0
	}
	n := 0
	for _, day := range report.Days {
		n += len(day.Conversations)
	}
	return n
}

// mapTotals aggregates the report-wide manifest totals.
func mapTotals(report *model.Report) ManifestTotals {
	return ManifestTotals{
		Conversations: ConversationCount(report),
		Invocations:   report.TotalStats.InvocationCount,
		InputTokens:   report.TotalStats.InputTokens,
		OutputTokens:  report.TotalStats.OutputTokens,
		Errors:        report.TotalStats.ErrorCount,
	}
}

// writeJSON marshals v as indented JSON and writes it to path. A trailing
// newline is appended so the files are clean to cat and diff.
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling JSON: %w", err)
	}
	b = append(b, '\n')
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// FormatDate formats a time as the YYYY-MM-DD date used in manifest date ranges.
// A zero time yields the empty string (an unbounded edge).
func FormatDate(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02")
}
