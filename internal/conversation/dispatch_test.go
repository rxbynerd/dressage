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
		{"vertex claude", "vertex", "claude-3-5-sonnet@20240620", familyAnthropic},
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

// A Gemini-on-Vertex record routes to the (unimplemented) Gemini family, which
// must return nil rather than silently decoding with the Anthropic decoder.
func TestReconstructGeminiStubReturnsNil(t *testing.T) {
	rec := model.Record{
		Provider:  "vertex",
		ModelID:   "gemini-2.0-flash",
		Timestamp: time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC),
		Input:     model.Body{JSON: []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)},
	}
	if got := Reconstruct([]model.Record{rec}); got != nil {
		t.Errorf("Reconstruct(gemini) = %+v, want nil (stub)", got)
	}
}
