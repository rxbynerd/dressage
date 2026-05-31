package conversation

import (
	"encoding/json"
	"log"
	"strings"
	"sync"

	"github.com/rxbynerd/dressage/internal/model"
)

type providerFamily int

const (
	familyAnthropic providerFamily = iota
	familyOpenAI
	familyGemini // Gemini contents[]/parts[] envelope (Vertex)
	// familyVertexDeferred marks non-Gemini Vertex models (notably Claude-on-
	// Vertex). These appear in summary stats but conversation reconstruction is
	// deferred to issue #4, so Reconstruct skips them with a one-line notice.
	familyVertexDeferred
)

// family maps a (provider, modelID) pair to the request/response envelope
// family used to reconstruct conversations. The model id is required because a
// single provider can serve multiple envelope families: Vertex hosts both Gemini
// (contents[]/parts[]) and Claude-on-Vertex (Anthropic Messages). Unknown
// providers default to Anthropic Messages.
func family(provider, modelID string) providerFamily {
	switch provider {
	case "azure":
		return familyOpenAI
	case "vertex":
		if strings.HasPrefix(modelID, "gemini") {
			return familyGemini
		}
		return familyVertexDeferred // Claude-on-Vertex etc.: deferred to #4
	default:
		return familyAnthropic
	}
}

// vertexDeferredOnce guards the per-run notice logged when non-Gemini Vertex
// records are encountered, so it is emitted once rather than once per conversation.
var vertexDeferredOnce sync.Once

// Reconstruct builds a ConversationDetail from records belonging to one
// conversation, dispatching on the provider's envelope family.
func Reconstruct(records []model.Record) *model.ConversationDetail {
	if len(records) == 0 {
		return nil
	}
	switch family(records[0].Provider, records[0].ModelID) {
	case familyOpenAI:
		return reconstructOpenAI(records)
	case familyGemini:
		return reconstructGemini(records)
	case familyVertexDeferred:
		vertexDeferredOnce.Do(func() {
			log.Printf("vertex: conversation reconstruction for non-Gemini models (e.g. %q) is deferred to issue #4; "+
				"these invocations still appear in summary stats", records[0].ModelID)
		})
		return nil
	default:
		return reconstructAnthropic(records)
	}
}

// ExtractSessionID returns the session id embedded in a request body, if any,
// using the provider-appropriate location.
func ExtractSessionID(provider, modelID string, inputBody json.RawMessage) string {
	switch family(provider, modelID) {
	case familyOpenAI:
		return extractSessionOpenAI(inputBody)
	case familyGemini:
		return extractSessionGemini(inputBody)
	default:
		return extractSessionAnthropic(inputBody)
	}
}
