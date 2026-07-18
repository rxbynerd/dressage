package ir

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Reader provides read access to an IR directory written by Export/Exporter. It
// loads and holds the manifest (small — one lightweight entry per conversation)
// and loads individual conversation files on demand, so a consumer never needs
// the whole directory in memory at once. Reader is the read-side counterpart to
// the Exporter: this package's only IO lives in export.go (writes) and here
// (reads); all model<->ir translation stays pure in map.go.
//
// A Reader is safe for concurrent use: after OpenDir it is read-only, and
// LoadConversation opens a fresh file per call.
type Reader struct {
	dir      string
	manifest *Manifest
	byName   map[string]*ManifestEntry
}

// OpenDir reads and validates dir/manifest.json and indexes its conversation
// entries by the URL-safe basename of their file field. It accepts any
// dressage.ir/1.x manifest (unknown newer minor fields are ignored) and rejects
// a different major version, whose layout this reader may not understand.
func OpenDir(dir string) (*Reader, error) {
	manifestPath := filepath.Join(dir, "manifest.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("reading IR manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parsing IR manifest %s: %w", manifestPath, err)
	}
	if err := checkSchemaMajor(m.SchemaVersion); err != nil {
		return nil, err
	}

	byName := make(map[string]*ManifestEntry, len(m.Conversations))
	for i := range m.Conversations {
		entry := &m.Conversations[i]
		byName[entry.Name()] = entry
	}
	return &Reader{dir: dir, manifest: &m, byName: byName}, nil
}

// Manifest returns the parsed manifest. The returned pointer is owned by the
// Reader; treat it as read-only.
func (r *Reader) Manifest() *Manifest {
	return r.manifest
}

// Lookup resolves a conversation's URL-safe name (the basename of its file
// without the .json extension, i.e. entryName) to its manifest entry.
func (r *Reader) Lookup(name string) (*ManifestEntry, bool) {
	e, ok := r.byName[name]
	return e, ok
}

// LoadConversation reads and parses the conversation file named by entry.File.
// The path is resolved strictly against the IR directory via the manifest's
// file field (never rebuilt from the raw id, which may contain separators), and
// a cleaned path that escapes the directory is rejected as defence in depth.
func (r *Reader) LoadConversation(entry *ManifestEntry) (*ConversationIR, error) {
	rel := filepath.FromSlash(entry.File)
	full := filepath.Join(r.dir, rel)

	// Defence in depth: the manifest file field is trusted (we wrote it), but a
	// tampered manifest must not read outside the IR directory.
	dirAbs, err := filepath.Abs(r.dir)
	if err != nil {
		return nil, fmt.Errorf("resolving IR directory: %w", err)
	}
	fullAbs, err := filepath.Abs(full)
	if err != nil {
		return nil, fmt.Errorf("resolving conversation path: %w", err)
	}
	if fullAbs != dirAbs && !strings.HasPrefix(fullAbs, dirAbs+string(filepath.Separator)) {
		return nil, fmt.Errorf("conversation file %q escapes the IR directory", entry.File)
	}

	raw, err := os.ReadFile(full)
	if err != nil {
		return nil, fmt.Errorf("reading conversation %s: %w", entry.File, err)
	}
	var conv ConversationIR
	if err := json.Unmarshal(raw, &conv); err != nil {
		return nil, fmt.Errorf("parsing conversation %s: %w", entry.File, err)
	}
	return &conv, nil
}

// Name is the URL-safe key for a conversation: the basename of its file with
// the .json extension trimmed. The on-disk names are already sanitized (no path
// separators; see sanitizeFilename), so this is a stable, routable handle that
// never needs re-escaping — a serve layer uses it directly as a path segment.
func (e ManifestEntry) Name() string {
	return strings.TrimSuffix(path.Base(e.File), ".json")
}

// checkSchemaMajor accepts a manifest whose schema version shares this build's
// major (the "1" in "dressage.ir/1.x"), and rejects anything else with a clear
// message. An empty or malformed version is treated as incompatible.
func checkSchemaMajor(version string) error {
	got := schemaMajor(version)
	want := schemaMajor(SchemaVersion)
	if got == "" {
		return fmt.Errorf("IR manifest has missing or malformed schema_version %q", version)
	}
	if got != want {
		return fmt.Errorf("IR manifest schema_version %q is incompatible with this build (%s); major %q != %q",
			version, SchemaVersion, got, want)
	}
	return nil
}

// schemaMajor extracts the MAJOR component from a "dressage.ir/MAJOR.MINOR"
// string, or "" when it does not match that shape.
func schemaMajor(version string) string {
	_, rest, ok := strings.Cut(version, "/")
	if !ok {
		return ""
	}
	major, _, ok := strings.Cut(rest, ".")
	if !ok || major == "" {
		return ""
	}
	return major
}
