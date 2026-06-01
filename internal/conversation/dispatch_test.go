package conversation

import (
	"testing"
	"time"

	"github.com/rxbynerd/dressage/internal/model"
)

func TestFamilyDispatch(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		modelID  string
		want     providerFamily
	}{
		{"azure is openai", "azure", "", familyOpenAI},
		{"vertex gemini", "vertex", "gemini-2.0-flash", familyGemini},
		{"vertex claude is deferred", "vertex", "claude-3-5-sonnet@20240620", familyVertexDeferred},
		{"bedrock defaults anthropic", "bedrock", "anthropic.claude-3", familyAnthropic},
		{"unknown defaults anthropic", "mystery", "x", familyAnthropic},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := family(tc.provider, tc.modelID); got != tc.want {
				t.Errorf("family(%q, %q) = %d, want %d", tc.provider, tc.modelID, got, tc.want)
			}
		})
	}
}

// A Gemini-on-Vertex record routes to the Gemini reconstructor, which decodes
// the contents[]/parts[] envelope rather than the Anthropic decoder.
func TestReconstructGeminiRoutes(t *testing.T) {
	rec := model.Record{
		Provider:  "vertex",
		ModelID:   "gemini-2.0-flash",
		Timestamp: time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC),
		Input:     model.Body{JSON: []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)},
	}
	got := Reconstruct([]model.Record{rec})
	if got == nil {
		t.Fatal("Reconstruct(gemini) = nil, want a reconstructed detail")
	}
	if len(got.Turns) != 1 || got.Turns[0].Role != "user" || got.Turns[0].Blocks[0].Text != "hi" {
		t.Errorf("Reconstruct(gemini) turns = %+v, want one user turn with text 'hi'", got.Turns)
	}
}

// A non-Gemini Vertex record (e.g. Claude-on-Vertex) is deferred: it must not be
// reconstructed here (tracked in #4), so Reconstruct returns nil.
func TestReconstructVertexClaudeDeferred(t *testing.T) {
	rec := model.Record{
		Provider:  "vertex",
		ModelID:   "claude-3-5-sonnet@20240620",
		Timestamp: time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC),
		Input:     model.Body{JSON: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)},
	}
	if got := Reconstruct([]model.Record{rec}); got != nil {
		t.Errorf("Reconstruct(vertex claude) = %+v, want nil (deferred to #4)", got)
	}
}
