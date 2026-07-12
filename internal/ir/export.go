package ir

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/parquet-go/parquet-go"

	"github.com/rxbynerd/dressage/internal/model"
)

// toolName identifies the producing tool in the manifest.
const toolName = "dressage"

// Version is the tool version stamped into the manifest's tool block. The CLI
// sets it from its build-time -ldflags version; it defaults to "dev" so the
// package is usable (and testable) without that wiring.
var Version = "dev"

// ExportOptions configures what an export includes.
type ExportOptions struct {
	// RawBodies embeds each invocation's verbatim request/response JSON at
	// invocations[].input.json / output.json. Off by default: resend-style
	// captures (the claude provider) resend the whole transcript every turn,
	// so embedded bodies grow quadratically with conversation length. The
	// manifest records the choice in its raw_bodies field.
	RawBodies bool
}

// rawBodiesMarker returns the manifest raw_bodies value for the options.
func (o ExportOptions) rawBodiesMarker() string {
	if o.RawBodies {
		return RawBodiesEmbedded
	}
	return RawBodiesOmitted
}

// Exporter writes an IR directory incrementally: conversations are written as
// they are produced (WriteConversation) and the manifest last (Finish), so a
// caller streaming conversations out of a summary.Plan never needs the whole
// report in memory. Export wraps it for callers that already hold a
// materialized report. The Exporter is the only IO in this package; all
// model.* -> ir.* translation stays pure in map.go (body payloads are loaded
// through the model's own Body.Load indirection).
type Exporter struct {
	dir       string
	manifest  Manifest
	opts      ExportOptions
	usedNames map[string]int
	facts     []FactRow
}

// NewExporter prepares dir for an export, creating the directory layout.
func NewExporter(dir string, src SourceInfo, generatedAt time.Time, opts ExportOptions) (*Exporter, error) {
	convDir := filepath.Join(dir, "conversations")
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating IR directory %s: %w", convDir, err)
	}
	return &Exporter{
		dir:  dir,
		opts: opts,
		manifest: Manifest{
			SchemaVersion: SchemaVersion,
			GeneratedAt:   generatedAt,
			Tool:          ToolInfo{Name: toolName, Version: Version},
			Source:        src,
			RawBodies:     opts.rawBodiesMarker(),
			// Non-nil so an empty report serializes "conversations": [] (a valid
			// empty array) rather than "conversations": null.
			Conversations: []ManifestEntry{},
		},
		// usedNames tracks the filesystem-safe filenames already written this
		// export so distinct ids that sanitize to the same name (e.g. "a/b" and
		// "a_b") get disambiguated rather than overwriting each other.
		usedNames: make(map[string]int),
	}, nil
}

// WriteConversation maps one conversation and writes its file, recording its
// manifest entry. The on-disk filename and the manifest `file` path derive
// from a filesystem-safe transform of the id; the id field itself stays raw.
func (e *Exporter) WriteConversation(cs model.ConversationSummary) error {
	conv := mapConversation(cs, e.opts)
	name := uniqueName(e.usedNames, sanitizeFilename(conv.ID))
	rel := "conversations/" + name + ".json"

	convPath := filepath.Join(e.dir, filepath.FromSlash(rel))
	// Defensive: guarantee the parent exists even though sanitizeFilename
	// yields a flat name (no separators) so it is always the conversations dir.
	if err := os.MkdirAll(filepath.Dir(convPath), 0o755); err != nil {
		return fmt.Errorf("creating conversation dir for %s: %w", conv.ID, err)
	}
	if err := writeJSON(convPath, conv); err != nil {
		return fmt.Errorf("writing conversation %s: %w", conv.ID, err)
	}
	e.manifest.Conversations = append(e.manifest.Conversations, mapEntry(conv, rel))
	// Facts rows are tiny (metadata only) — buffering them all and sorting at
	// Finish keeps the table deterministic regardless of conversation order.
	e.facts = append(e.facts, mapFacts(cs)...)
	return nil
}

// Finish writes the columnar tables, sorts the manifest index, and writes
// manifest.json. The report supplies the run-wide totals (its stats are
// complete even when conversations were streamed rather than retained).
func (e *Exporter) Finish(report *model.Report) error {
	sort.SliceStable(e.facts, func(i, j int) bool {
		a, b := e.facts[i], e.facts[j]
		if !a.Timestamp.Equal(b.Timestamp) {
			return a.Timestamp.Before(b.Timestamp)
		}
		if a.RequestID != b.RequestID {
			return a.RequestID < b.RequestID
		}
		return a.RequestUUID < b.RequestUUID
	})
	if err := writeParquet(filepath.Join(e.dir, FactsFile), e.facts); err != nil {
		return fmt.Errorf("writing facts table: %w", err)
	}
	e.manifest.Files.Facts = FactsFile

	e.manifest.Totals = mapTotals(report.TotalStats, len(e.manifest.Conversations))

	// Sort the index for byte-stable output: by start time, then id. Stable so
	// conversations with an identical (start_time, id) keep their walk order.
	sort.SliceStable(e.manifest.Conversations, func(i, j int) bool {
		a, b := e.manifest.Conversations[i], e.manifest.Conversations[j]
		if !a.StartTime.Equal(b.StartTime) {
			return a.StartTime.Before(b.StartTime)
		}
		return a.ID < b.ID
	})

	if err := writeJSON(filepath.Join(e.dir, "manifest.json"), e.manifest); err != nil {
		return fmt.Errorf("writing manifest: %w", err)
	}
	return nil
}

// Export writes the IR for a report to dir: one JSON file per conversation under
// dir/conversations/, plus a manifest.json index at dir's root. It mirrors
// report.Generate's role — it takes the same *model.Report and a destination.
//
// Output is deterministic: manifest.conversations is sorted by start time then
// id, every file is indented JSON, and file names derive from the stable id, so
// two runs over the same report produce byte-identical output (enabling golden
// tests and clean diffs).
func Export(report *model.Report, dir string, src SourceInfo, opts ExportOptions) error {
	if report == nil {
		return fmt.Errorf("ir export: nil report")
	}
	e, err := NewExporter(dir, src, report.GeneratedAt, opts)
	if err != nil {
		return err
	}
	for _, day := range report.Days {
		for _, cs := range day.Conversations {
			if err := e.WriteConversation(cs); err != nil {
				return err
			}
		}
	}
	return e.Finish(report)
}

// uniqueName returns name unchanged the first time it is seen, then name_2,
// name_3, … on subsequent collisions, recording each result so a later identical
// base cannot collide with the disambiguated form either. used is mutated.
func uniqueName(used map[string]int, name string) string {
	candidate := name
	for {
		n := used[candidate]
		used[candidate] = n + 1
		if n == 0 {
			return candidate
		}
		candidate = fmt.Sprintf("%s_%d", name, n+1)
	}
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

// mapTotals aggregates the run-wide manifest totals from the report stats and
// the number of conversations actually written (which the skeleton report of a
// streaming run does not know).
func mapTotals(stats model.Stats, conversations int) ManifestTotals {
	return ManifestTotals{
		Conversations: conversations,
		Invocations:   stats.InvocationCount,
		InputTokens:   stats.InputTokens,
		OutputTokens:  stats.OutputTokens,
		Errors:        stats.ErrorCount,
	}
}

// writeParquet writes rows as a zstd-compressed Parquet file at path. Note
// that Parquet bytes are NOT stable across parquet-go versions (footer
// metadata embeds the library); tests assert on decoded rows, not file bytes.
func writeParquet[T any](path string, rows []T) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	w := parquet.NewGenericWriter[T](f, parquet.Compression(&parquet.Zstd))
	if len(rows) > 0 {
		if _, err := w.Write(rows); err != nil {
			f.Close()
			return fmt.Errorf("writing rows to %s: %w", path, err)
		}
	}
	if err := w.Close(); err != nil {
		f.Close()
		return fmt.Errorf("finalizing %s: %w", path, err)
	}
	return f.Close()
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

// FormatDate formats t for use in the IR manifest's date-range fields
// (YYYY-MM-DD). A zero time yields "", indicating an unbounded range edge. It is
// exported for downstream programs that construct a SourceInfo outside this
// package and want manifest-consistent date formatting.
func FormatDate(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02")
}
