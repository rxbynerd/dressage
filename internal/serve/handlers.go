package serve

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/rxbynerd/dressage/internal/ir"
)

// handleIndex renders the landing page: run header, per-day cards, and the
// conversation list — all from the manifest, opening no conversation file.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	view := buildIndexView(s.reader.Manifest())
	render(w, indexTmpl, view)
}

// handleConversation renders one conversation, loaded on demand by its URL-safe
// name (the sanitized file basename recorded in the manifest).
func (s *Server) handleConversation(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("conv")
	entry, ok := s.reader.Lookup(name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	conv, err := s.reader.LoadConversation(entry)
	if err != nil {
		http.Error(w, "loading conversation: "+err.Error(), http.StatusInternalServerError)
		return
	}
	embedded := s.reader.Manifest().RawBodies == ir.RawBodiesEmbedded
	render(w, convTmpl, buildConvView(conv, name, embedded))
}

// handleRawBody streams one invocation's verbatim request or response JSON as
// application/json. It 404s when the name/index is unknown, the direction is
// not input/output, or the body was not embedded in this export.
func (s *Server) handleRawBody(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("conv")
	dir := r.PathValue("dir")
	if dir != "input" && dir != "output" {
		http.NotFound(w, r)
		return
	}
	idx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil || idx < 0 {
		http.NotFound(w, r)
		return
	}

	entry, ok := s.reader.Lookup(name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	conv, err := s.reader.LoadConversation(entry)
	if err != nil {
		http.Error(w, "loading conversation: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if idx >= len(conv.Invocations) {
		http.NotFound(w, r)
		return
	}

	body := conv.Invocations[idx].Input
	if dir == "output" {
		body = conv.Invocations[idx].Output
	}
	if len(body.JSON) == 0 {
		// Either bodies were not embedded, or this particular body was absent.
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Indent for readability; fall back to the raw bytes if that fails.
	if indented, err := indentJSON(body.JSON); err == nil {
		_, _ = w.Write(indented)
		return
	}
	_, _ = w.Write(body.JSON)
}

// indentJSON pretty-prints raw JSON, returning an error when it is not valid
// JSON so the caller can fall back to the verbatim bytes.
func indentJSON(raw json.RawMessage) ([]byte, error) {
	return json.MarshalIndent(json.RawMessage(raw), "", "  ")
}
