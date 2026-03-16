// Package report generates a self-contained HTML report from Bedrock log summaries.
package report

import (
	"embed"
	"fmt"
	"html/template"
	"io"
	"os"
	"strings"
	"time"

	"github.com/rubynerd/dressage/internal/model"
)

//go:embed template.html
var templateFS embed.FS

var funcMap = template.FuncMap{
	"formatInt":     formatInt,
	"shortSession":  shortSession,
	"shortModel":    shortModel,
	"truncate":      truncate,
	"durationMs":    durationMs,
	"hasPrefix":     strings.HasPrefix,
	"turnHasBlocks": turnHasBlocks,
	"sub":           func(a, b int) int { return a - b },
	"add":           func(a, b int) int { return a + b },
	"timeFmt":       func(t time.Time, f string) string { return t.Format(f) },
}

// formatInt formats an int64 with thousands separators.
func formatInt(n int64) string {
	if n < 0 {
		return "-" + formatInt(-n)
	}
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var result []byte
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, byte(c))
	}
	return string(result)
}

// shortSession returns the first 8 chars of a session ID for display.
func shortSession(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// shortModel extracts a short model name from a full ARN or model ID.
func shortModel(s string) string {
	// Handle ARN format: arn:aws:bedrock:...:inference-profile/eu.anthropic.claude-opus-4-6-v1
	if idx := strings.LastIndex(s, "/"); idx >= 0 {
		s = s[idx+1:]
	}
	// Strip region prefixes like "eu." or "us."
	if len(s) > 3 && s[2] == '.' {
		s = s[3:]
	}
	// Strip version suffixes like "-v1"
	if strings.HasSuffix(s, "-v1") {
		s = s[:len(s)-3]
	}
	return s
}

// truncate limits a string to n characters, appending "..." if truncated.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// durationMs formats milliseconds as a human-readable duration.
func durationMs(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return fmt.Sprintf("%.1fs", float64(ms)/1000)
}

// turnHasBlocks returns true if the turn has any blocks of the given type.
func turnHasBlocks(turn model.Turn, blockType string) bool {
	for _, b := range turn.Blocks {
		if b.Type == blockType {
			return true
		}
	}
	return false
}

// Generate writes the HTML report to the specified file path.
func Generate(report *model.Report, outputPath string) error {
	f, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("creating output file: %w", err)
	}
	defer f.Close()

	return Render(report, f)
}

// Render writes the HTML report to the given writer.
func Render(report *model.Report, w io.Writer) error {
	tmplData, err := templateFS.ReadFile("template.html")
	if err != nil {
		return fmt.Errorf("reading embedded template: %w", err)
	}

	tmpl, err := template.New("report").Funcs(funcMap).Parse(string(tmplData))
	if err != nil {
		return fmt.Errorf("parsing template: %w", err)
	}

	if err := tmpl.Execute(w, report); err != nil {
		return fmt.Errorf("executing template: %w", err)
	}

	return nil
}
