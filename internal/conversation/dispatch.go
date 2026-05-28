package conversation

import (
	"encoding/json"

	"github.com/rxbynerd/dressage/internal/model"
)

type providerFamily int

const (
	familyAnthropic providerFamily = iota
	familyOpenAI
)

// family maps a provider id to the request/response envelope family used to
// reconstruct conversations. Unknown providers default to Anthropic Messages.
func family(provider string) providerFamily {
	switch provider {
	case "azure":
		return familyOpenAI
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
	switch family(records[0].Provider) {
	default:
		return reconstructAnthropic(records)
	}
}

// ExtractSessionID returns the session id embedded in a request body, if any,
// using the provider-appropriate location.
func ExtractSessionID(provider string, inputBody json.RawMessage) string {
	switch family(provider) {
	default:
		return extractSessionAnthropic(inputBody)
	}
}
