// Package serve presents an IR directory as a browsable, localhost-only web UI.
// It reads exclusively through an ir.Reader — the manifest for the index page
// and one conversation file per request — so memory stays bounded to a single
// conversation regardless of how large the capture is. The pages are
// server-rendered Go html/template output reusing the retired HTML report's
// styling, with <details>/<summary> drill-down and no client-side JavaScript.
//
// It is a local developer tool: no authentication, no TLS, no rate limiting.
// Bind it to a loopback address.
package serve

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"

	"github.com/rxbynerd/dressage/internal/ir"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

// funcMap exposes the display formatters (ported from the HTML report) plus the
// two body pretty-printers to the templates.
var funcMap = template.FuncMap{
	"formatInt":    formatInt,
	"shortModel":   shortModel,
	"shortSession": shortSession,
	"truncate":     truncate,
	"durationMs":   durationMs,
	"prettyJSON":   prettyJSON,
	"prettyBody":   prettyBody,
	"hasJSON":      func(raw json.RawMessage) bool { return len(raw) > 0 },
}

// base templates are shared by every page; each page file additionally defines
// the "content" block that base renders.
var baseFiles = []string{"templates/base.html.tmpl", "templates/_convview.html.tmpl"}

var (
	indexTmpl = template.Must(template.New("").Funcs(funcMap).
			ParseFS(templateFS, append(baseFiles, "templates/index.html.tmpl")...))
	convTmpl = template.Must(template.New("").Funcs(funcMap).
			ParseFS(templateFS, append(baseFiles, "templates/conversation.html.tmpl")...))
)

// Server renders an IR directory over HTTP. It is read-only after construction
// and safe for concurrent requests.
type Server struct {
	reader *ir.Reader
}

// New builds a Server over an opened IR directory.
func New(reader *ir.Reader) *Server {
	return &Server{reader: reader}
}

// Handler returns the HTTP handler for the server's routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /conversations/{conv}", s.handleConversation)
	mux.HandleFunc("GET /conversations/{conv}/raw/{idx}/{dir}", s.handleRawBody)
	return mux
}

// ListenAndServe serves the UI on addr until the process exits or the listener
// errors. addr should be a loopback bind (e.g. "127.0.0.1:7878").
func (s *Server) ListenAndServe(addr string) error {
	return http.ListenAndServe(addr, s.Handler())
}

// render executes a page template into a buffer first, so a template error
// produces a clean 500 rather than a half-written 200 response.
func render(w http.ResponseWriter, tmpl *template.Template, data any) {
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "base", data); err != nil {
		http.Error(w, fmt.Sprintf("render error: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}
