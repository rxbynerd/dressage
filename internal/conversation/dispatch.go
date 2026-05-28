package conversation

import (
	"encoding/json"
	"strings"

	"github.com/rxbynerd/dressage/internal/model"
)

type providerFamily int

const (
	familyAnthropic providerFamily = iota
	familyOpenAI
	familyGemini // Gemini contents[]/parts[] envelope — reconstruction implemented in #6
)

// family maps a (provider, modelID) pair to the request/response envelope
// family used to reconstruct conversations. The model id is required because a
// single provider can serve multiple envelope families: Vertex (#6/#4) hosts
// both Gemini (contents[]/parts[]) and Claude-on-Vertex (Anthropic Messages).
// Unknown providers default to Anthropic Messages.
func family(provider, modelID string) providerFamily {
	switch provider {
	case "azure":
		return familyOpenAI
	case "vertex":
		if strings.HasPrefix(modelID, "gemini") {
			return familyGemini
		}
		return familyAnthropic // Claude-on-Vertex (#4) uses the Anthropic Messages envelope
	default:
		return familyAnthropic
	}
}

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
		return nil // implemented in #6
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
	default:
		return extractSessionAnthropic(inputBody)
	}
}
