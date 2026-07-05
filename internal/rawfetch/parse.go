package rawfetch

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rxbynerd/dressage/internal/conversation"
	"github.com/rxbynerd/dressage/internal/model"
)

// parsedRequest holds the fields extracted from one request body plus its raw
// payload (retained because conversation reconstruction reads it in full).
type parsedRequest struct {
	uuid        string // request-file basename without the .request.json suffix
	mtime       time.Time
	model       string
	sessionID   string
	prevID      string // diagnostics.previous_message_id ("" on the first turn)
	numMessages int
	accountUUID string
	deviceID    string
	raw         json.RawMessage
}

// responseMeta is the lightweight index entry for one response body. The full
// payload is intentionally not retained; it is re-read from path only if the
// response is paired to a request.
type responseMeta struct {
	msgID        string
	apiReqID     string // "req_..." id parsed from the filename
	path         string
	mtime        time.Time
	model        string
	inputTokens  int64
	outputTokens int64
	cacheRead    int64
	cacheWrite   int64
	claimed      bool
}

// reqEnvelope decodes just the request fields needed for correlation and
// identity. messages is decoded into zero-size elements so only its length is
// retained, not the (potentially huge) message contents.
type reqEnvelope struct {
	Model    string     `json:"model"`
	Messages []struct{} `json:"messages"`
	Metadata struct {
		UserID string `json:"user_id"`
	} `json:"metadata"`
	Diagnostics struct {
		PreviousMessageID string `json:"previous_message_id"`
	} `json:"diagnostics"`
}

// userIDBlob decodes the JSON object form of metadata.user_id written by the
// direct Anthropic API path.
type userIDBlob struct {
	DeviceID    string `json:"device_id"`
	AccountUUID string `json:"account_uuid"`
	SessionID   string `json:"session_id"`
}

// respEnvelope decodes the response fields needed for the index; content is
// deliberately omitted so it is skipped rather than retained.
type respEnvelope struct {
	ID    string `json:"id"`
	Model string `json:"model"`
	Usage struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

// parseRequests decodes every request file concurrently, returning the parsed
// requests. Files that fail to read or decode, or that carry no messages, are
// skipped (they are not valid Messages API requests).
func parseRequests(ctx context.Context, paths []timedPath, workers int) ([]parsedRequest, error) {
	var (
		mu   sync.Mutex
		out  []parsedRequest
		skip int
	)
	err := forEachPath(ctx, paths, workers, func(tp timedPath) error {
		data, err := os.ReadFile(tp.path)
		if err != nil {
			mu.Lock()
			skip++
			mu.Unlock()
			return nil
		}
		var env reqEnvelope
		if err := json.Unmarshal(data, &env); err != nil || len(env.Messages) == 0 {
			mu.Lock()
			skip++
			mu.Unlock()
			return nil
		}

		blob := parseUserID(env.Metadata.UserID)
		pr := parsedRequest{
			uuid:        strings.TrimSuffix(filepath.Base(tp.path), ".request.json"),
			mtime:       tp.mtime,
			model:       env.Model,
			sessionID:   conversation.ExtractSessionID(provider, env.Model, data),
			prevID:      env.Diagnostics.PreviousMessageID,
			numMessages: len(env.Messages),
			accountUUID: blob.AccountUUID,
			deviceID:    blob.DeviceID,
			raw:         json.RawMessage(data),
		}
		mu.Lock()
		out = append(out, pr)
		mu.Unlock()
		return nil
	})
	if err != nil {
		return nil, err
	}
	if skip > 0 {
		log.Printf("%s: skipped %d request file(s) that could not be read or decoded", provider, skip)
	}
	return out, nil
}

// indexResponses decodes every response file concurrently into a map keyed by
// message id. Duplicate ids keep the earliest-written entry for determinism.
func indexResponses(ctx context.Context, paths []timedPath, workers int) (map[string]*responseMeta, error) {
	var (
		mu    sync.Mutex
		index = make(map[string]*responseMeta, len(paths))
	)
	err := forEachPath(ctx, paths, workers, func(tp timedPath) error {
		data, err := os.ReadFile(tp.path)
		if err != nil {
			return nil // skip unreadable response
		}
		var env respEnvelope
		if err := json.Unmarshal(data, &env); err != nil || env.ID == "" {
			return nil
		}
		rm := &responseMeta{
			msgID:        env.ID,
			apiReqID:     strings.TrimSuffix(filepath.Base(tp.path), ".response.json"),
			path:         tp.path,
			mtime:        tp.mtime,
			model:        env.Model,
			inputTokens:  env.Usage.InputTokens,
			outputTokens: env.Usage.OutputTokens,
			cacheRead:    env.Usage.CacheReadInputTokens,
			cacheWrite:   env.Usage.CacheCreationInputTokens,
		}
		mu.Lock()
		if existing, ok := index[env.ID]; !ok || tp.mtime.Before(existing.mtime) {
			index[env.ID] = rm
		}
		mu.Unlock()
		return nil
	})
	if err != nil {
		return nil, err
	}
	return index, nil
}

// parseUserID decodes the JSON object form of metadata.user_id. It returns a
// zero value when the field is empty or is the flat legacy identity string
// (which carries no account/device attributes).
func parseUserID(userID string) userIDBlob {
	trimmed := strings.TrimSpace(userID)
	if !strings.HasPrefix(trimmed, "{") {
		return userIDBlob{}
	}
	var blob userIDBlob
	if err := json.Unmarshal([]byte(trimmed), &blob); err != nil {
		return userIDBlob{}
	}
	return blob
}

// buildRecords pairs parsed requests with their response bodies and materializes
// the normalized records. Pairing within a session uses the message-id chain
// (turn i's response is the body named by turn i+1's previous_message_id); each
// session's terminal turn is matched by write time. Requests are returned sorted
// by (mtime, uuid) for deterministic output.
func buildRecords(reqs []parsedRequest, respIndex map[string]*responseMeta) []model.Record {
	// paired[i] is the response chosen for reqs[i], or nil if none.
	paired := make([]*responseMeta, len(reqs))

	// Group request indices by session so message-count ordering is per-session.
	sessions := make(map[string][]int)
	order := make([]string, 0)
	for i := range reqs {
		sid := reqs[i].sessionID
		if _, ok := sessions[sid]; !ok {
			order = append(order, sid)
		}
		sessions[sid] = append(sessions[sid], i)
	}
	sort.Strings(order)

	var terminals []int // request indices needing the write-time fallback
	for _, sid := range order {
		idxs := sessions[sid]
		sort.Slice(idxs, func(a, b int) bool {
			ra, rb := reqs[idxs[a]], reqs[idxs[b]]
			if ra.numMessages != rb.numMessages {
				return ra.numMessages < rb.numMessages
			}
			return ra.mtime.Before(rb.mtime)
		})
		// Turn i's response is named by turn i+1's previous_message_id.
		for pos := 0; pos+1 < len(idxs); pos++ {
			prev := reqs[idxs[pos+1]].prevID
			if prev == "" {
				continue
			}
			if rm, ok := respIndex[prev]; ok && !rm.claimed {
				rm.claimed = true
				paired[idxs[pos]] = rm
			}
		}
		// The terminal turn has no successor to name its response.
		terminals = append(terminals, idxs[len(idxs)-1])
	}

	matchTerminals(reqs, terminals, respIndex, paired)

	records := make([]model.Record, 0, len(reqs))
	for i := range reqs {
		records = append(records, makeRecord(reqs[i], paired[i]))
	}
	sort.Slice(records, func(a, b int) bool {
		if !records[a].Timestamp.Equal(records[b].Timestamp) {
			return records[a].Timestamp.Before(records[b].Timestamp)
		}
		return records[a].RequestID < records[b].RequestID
	})
	return records
}

// matchTerminals assigns each terminal request the earliest unclaimed response
// of the same model written at or after the request, within terminalMatchWindow.
// Terminals are processed in write-time order so earlier turns claim their
// nearest response first.
func matchTerminals(reqs []parsedRequest, terminals []int, respIndex map[string]*responseMeta, paired []*responseMeta) {
	// Collect responses still unclaimed after id-based pairing, sorted by mtime.
	var free []*responseMeta
	for _, rm := range respIndex {
		if !rm.claimed {
			free = append(free, rm)
		}
	}
	if len(free) == 0 {
		return
	}
	sort.Slice(free, func(a, b int) bool { return free[a].mtime.Before(free[b].mtime) })

	sort.Slice(terminals, func(a, b int) bool {
		return reqs[terminals[a]].mtime.Before(reqs[terminals[b]].mtime)
	})

	for _, ti := range terminals {
		r := reqs[ti]
		deadline := r.mtime.Add(terminalMatchWindow)
		for _, rm := range free {
			if rm.claimed {
				continue
			}
			if rm.mtime.Before(r.mtime) {
				continue // response predates the request; cannot be its reply
			}
			if rm.mtime.After(deadline) {
				break // free is sorted by mtime; nothing further is in-window
			}
			if r.model != "" && rm.model != "" && r.model != rm.model {
				continue
			}
			rm.claimed = true
			paired[ti] = rm
			break
		}
	}
}

// makeRecord assembles one normalized record from a parsed request and its
// optional paired response. Token accounting and the API request id come from
// the response; when unpaired only the request-side fields are populated.
func makeRecord(pr parsedRequest, rm *responseMeta) model.Record {
	rec := model.Record{
		Provider:  provider,
		Timestamp: pr.mtime.UTC(),
		RequestID: pr.uuid,
		ModelID:   pr.model,
		Operation: operation,
		Identity: model.Identity{
			Principal: pr.accountUUID,
			Extra:     identityExtra(pr),
		},
		Input: model.Body{JSON: pr.raw, ContentType: "application/json"},
	}
	if rm == nil {
		return rec
	}

	rec.Status = "200"
	rec.RequestID = rm.apiReqID
	rec.Input.TokenCount = rm.inputTokens
	rec.Input.CacheRead = rm.cacheRead
	rec.Input.CacheWrite = rm.cacheWrite
	rec.Output = model.Body{ContentType: "application/json", TokenCount: rm.outputTokens}
	if body, err := os.ReadFile(rm.path); err == nil {
		rec.Output.JSON = json.RawMessage(body)
	} else {
		log.Printf("%s: response body %s vanished before read; turn content omitted", provider, rm.path)
	}
	return rec
}

// identityExtra builds the per-record identity attribute map, omitting empty
// values so the report does not render blank fields.
func identityExtra(pr parsedRequest) map[string]string {
	extra := make(map[string]string, 2)
	if pr.sessionID != "" {
		extra["session_id"] = pr.sessionID
	}
	if pr.deviceID != "" {
		extra["device_id"] = pr.deviceID
	}
	if len(extra) == 0 {
		return nil
	}
	return extra
}
