package rawfetch

import (
	"encoding/json"
	"os"
)

// fileBody is a model.BodySource backed by a capture file, so records carry a
// path instead of holding every transcript resident (the memory sink for
// resend-style captures). For request bodies it also surfaces the message
// count recorded at index time as the MessageCount hint, letting conversation
// reconstruction pick the fullest request without loading any payload.
type fileBody struct {
	path     string
	messages int // request message count; -1 for response bodies
}

// Load reads the capture file. A file that vanished between indexing and use
// surfaces as an error; consumers degrade that body rather than aborting.
func (f fileBody) Load() (json.RawMessage, error) {
	data, err := os.ReadFile(f.path)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(data), nil
}

// MessageCount reports the request's transcript length, when known.
func (f fileBody) MessageCount() (int, bool) {
	if f.messages < 0 {
		return 0, false
	}
	return f.messages, true
}
