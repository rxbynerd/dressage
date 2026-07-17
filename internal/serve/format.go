package serve

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// The display formatters below are ported verbatim from the retired HTML report
// (internal/report) so the served pages read identically. shortModel/
// shortSession/truncate/durationMs/formatInt keep their original semantics.

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
	s = strings.TrimSuffix(s, "-v1")
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

// prettyJSON pretty-prints inline JSON for display, passing the raw bytes
// through unchanged when they are not valid JSON. Empty input yields "". Used
// for small embedded values (tool inputs) where no size cap is needed.
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

// maxRenderedBodyBytes bounds the pretty-printed size of a raw request/response
// body shown inline on a conversation page, so a resend-style provider's
// multi-MB transcript body does not make the page slow to render or scroll. The
// full body is always available via the raw-download endpoint; this is only the
// inline preview cap.
const maxRenderedBodyBytes = 32 * 1024

// prettyBody pretty-prints a raw invocation body for inline display, truncating
// to maxRenderedBodyBytes on a UTF-8 rune boundary with a marker noting the
// original size. Returns "" for an absent body.
func prettyBody(raw json.RawMessage) string {
	s := prettyJSON(raw)
	if len(s) <= maxRenderedBodyBytes {
		return s
	}
	cut := maxRenderedBodyBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return fmt.Sprintf("%s\n\n… truncated (%d of %d bytes shown; use the raw link above for the full body)",
		s[:cut], cut, len(s))
}
