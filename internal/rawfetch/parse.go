package rawfetch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// parsedRequest holds the fields extracted from one request body. The payload
// itself is NOT retained — records carry a file-backed lazy source instead, so
// the resident set stays proportional to the number of requests rather than
// the (quadratically growing) transcript bytes.
type parsedRequest struct {
	uuid        string // request-file basename without the .request.json suffix
	path        string
	mtime       time.Time
	model       string
	sessionID   string
	prevID      string // diagnostics.previous_message_id ("" on the first turn)
	chainKey    string // hash of messages[0]; requests of one linear thread share it
	numMessages int
	accountUUID string
	deviceID    string
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
	stopReason   string
	inputTokens  int64
	outputTokens int64
	cacheRead    int64
	cacheWrite   int64
	claimed      bool
}

// reqEnvelope decodes just the request fields needed for correlation and
// identity. messages elements are kept raw and unexamined — only their count
// and the first element (hashed into the chain key) are used; the decoded
// slice is released when the envelope goes out of scope.
type reqEnvelope struct {
	Model    string            `json:"model"`
	Messages []json.RawMessage `json:"messages"`
	Metadata struct {
		UserID string `json:"user_id"`
	} `json:"metadata"`
	Diagnostics struct {
		PreviousMessageID string `json:"previous_message_id"`
	} `json:"diagnostics"`
}

// chainKeyOf hashes a request's first message into its chain key. A linear
// thread's first message is invariant as the transcript grows (the opening
// user prompt for the main thread, the task prompt for a subagent), so growing
// prefixes of one thread share a key while parallel threads in the same
// session split. The message is compacted first so formatting differences
// cannot split a chain.
func chainKeyOf(first json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, first); err != nil {
		sum := sha256.Sum256(first)
		return hex.EncodeToString(sum[:])
	}
	sum := sha256.Sum256(buf.Bytes())
	return hex.EncodeToString(sum[:])
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
	ID         string `json:"id"`
	Model      string `json:"model"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
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
			path:        tp.path,
			mtime:       tp.mtime,
			model:       env.Model,
			sessionID:   conversation.ExtractSessionID(provider, env.Model, data),
			prevID:      env.Diagnostics.PreviousMessageID,
			chainKey:    chainKeyOf(env.Messages[0]),
			numMessages: len(env.Messages),
			accountUUID: blob.AccountUUID,
			deviceID:    blob.DeviceID,
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
			stopReason:   env.StopReason,
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
// the normalized records. Each session's requests are split into chains (linear
// threads: the main thread and each subagent sidechain), pairing runs per chain
// via the message-id links (turn i's response is the body named by turn i+1's
// previous_message_id), and each chain's terminal turn is matched by write
// time. Requests are returned sorted by (mtime, uuid) for deterministic output.
func buildRecords(reqs []parsedRequest, respIndex map[string]*responseMeta) []model.Record {
	// paired[i] is the response chosen for reqs[i], or nil if none.
	paired := make([]*responseMeta, len(reqs))
	// threadIDs[i] is the thread (chain) reqs[i] belongs to.
	threadIDs := make([]string, len(reqs))

	// Group request indices by session; chain splitting is per-session.
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

	var terminals []int // unpaired chain tips needing the write-time fallback
	for _, sid := range order {
		terminals = append(terminals, pairSession(reqs, sessions[sid], respIndex, paired, threadIDs)...)
	}

	matchTerminals(reqs, terminals, respIndex, paired)

	records := make([]model.Record, 0, len(reqs))
	for i := range reqs {
		records = append(records, makeRecord(reqs[i], paired[i], threadIDs[i]))
	}
	sort.Slice(records, func(a, b int) bool {
		if !records[a].Timestamp.Equal(records[b].Timestamp) {
			return records[a].Timestamp.Before(records[b].Timestamp)
		}
		return records[a].RequestID < records[b].RequestID
	})
	return records
}

// splitChains groups one session's request indices into chains by chain key.
// Growing prefixes of one linear thread share the key, so the main thread and
// each subagent sidechain land in separate chains even when their turns
// interleave in time. Each chain is ordered by (message count, mtime); chains
// are returned in root-mtime order (ties: root uuid) so the earliest-started
// thread comes first and processing is deterministic.
func splitChains(reqs []parsedRequest, idxs []int) [][]int {
	groups := make(map[string][]int)
	keys := make([]string, 0)
	for _, i := range idxs {
		k := reqs[i].chainKey
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], i)
	}

	chains := make([][]int, 0, len(keys))
	for _, k := range keys {
		c := groups[k]
		sort.Slice(c, func(a, b int) bool {
			ra, rb := reqs[c[a]], reqs[c[b]]
			if ra.numMessages != rb.numMessages {
				return ra.numMessages < rb.numMessages
			}
			return ra.mtime.Before(rb.mtime)
		})
		chains = append(chains, c)
	}
	sort.Slice(chains, func(a, b int) bool {
		ra, rb := reqs[chains[a][0]], reqs[chains[b][0]]
		if !ra.mtime.Equal(rb.mtime) {
			return ra.mtime.Before(rb.mtime)
		}
		return ra.uuid < rb.uuid
	})
	return chains
}

// pairSession pairs one session's requests with responses and assigns thread
// ids. Requests split into chains, each chain pairs by message-id adjacency,
// and compaction continuations — a chain whose root names another chain's
// response — are stitched into that chain's thread. The thread id is the root
// chain's first request uuid. It returns the chain tips still unpaired, for
// the write-time fallback.
func pairSession(reqs []parsedRequest, idxs []int, respIndex map[string]*responseMeta, paired []*responseMeta, threadIDs []string) []int {
	chains := splitChains(reqs, idxs)

	// Within a chain, turn i's response is named by turn i+1's
	// previous_message_id. (A non-root request's predecessor is in its own
	// chain by construction: both carry the same first message.)
	for _, c := range chains {
		for pos := 0; pos+1 < len(c); pos++ {
			prev := reqs[c[pos+1]].prevID
			if prev == "" {
				continue
			}
			if rm, ok := respIndex[prev]; ok && !rm.claimed {
				rm.claimed = true
				paired[c[pos]] = rm
			}
		}
	}

	// respChain: paired response message id → index of the chain that received
	// it, for resolving compaction links below.
	respChain := make(map[string]int)
	for ci, c := range chains {
		for _, ri := range c {
			if rm := paired[ri]; rm != nil {
				respChain[rm.msgID] = ci
			}
		}
	}

	// Compaction rewrites the transcript, so the continuation arrives as a new
	// chain whose ROOT names the old context's final response. Stitch such a
	// chain into the named response's thread; when that response is still
	// unclaimed, it also pairs to the nearest preceding unpaired chain tip (the
	// old context's final request). Chains are in root-mtime order, so an
	// owner's thread root is final before any continuation of it is processed.
	rootOf := make([]int, len(chains))
	for ci := range chains {
		rootOf[ci] = ci
	}
	for ci, c := range chains {
		prev := reqs[c[0]].prevID
		if prev == "" {
			continue
		}
		owner, ok := respChain[prev]
		if !ok {
			if rm, exists := respIndex[prev]; exists && !rm.claimed {
				if oi, tip := precedingUnpairedTip(reqs, chains, paired, ci, rm); oi >= 0 {
					rm.claimed = true
					paired[tip] = rm
					respChain[rm.msgID] = oi
					owner, ok = oi, true
				}
			}
		}
		if ok && owner != ci {
			rootOf[ci] = rootOf[owner]
		}
	}

	var tips []int
	for ci, c := range chains {
		tid := reqs[chains[rootOf[ci]][0]].uuid
		for _, ri := range c {
			threadIDs[ri] = tid
		}
		if tip := c[len(c)-1]; paired[tip] == nil {
			tips = append(tips, tip)
		}
	}
	return tips
}

// precedingUnpairedTip finds the chain (before index `before`, in root-mtime
// order) whose still-unpaired tip most plausibly produced rm: written at or
// before rm, same model when both are known, latest such tip winning. Returns
// (-1, -1) when no chain qualifies.
func precedingUnpairedTip(reqs []parsedRequest, chains [][]int, paired []*responseMeta, before int, rm *responseMeta) (chainIdx, tipIdx int) {
	chainIdx, tipIdx = -1, -1
	var bestMtime time.Time
	for ci := range before {
		tip := chains[ci][len(chains[ci])-1]
		if paired[tip] != nil {
			continue
		}
		r := reqs[tip]
		if r.mtime.After(rm.mtime) {
			continue
		}
		if r.model != "" && rm.model != "" && r.model != rm.model {
			continue
		}
		if chainIdx < 0 || r.mtime.After(bestMtime) {
			chainIdx, tipIdx, bestMtime = ci, tip, r.mtime
		}
	}
	return chainIdx, tipIdx
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

// makeRecord assembles one normalized record from a parsed request, its
// optional paired response, and its thread id. Both bodies are file-backed
// lazy sources — makeRecord performs no IO; payloads are read only when a
// consumer actually needs them. Token accounting, the API request id, the
// response message id and the stop reason come from the response envelope
// decoded at index time; when unpaired only the request-side fields are
// populated.
func makeRecord(pr parsedRequest, rm *responseMeta, threadID string) model.Record {
	rec := model.Record{
		Provider:  provider,
		Timestamp: pr.mtime.UTC(),
		RequestID: pr.uuid,
		ModelID:   pr.model,
		Operation: operation,
		SessionID: pr.sessionID,
		Identity: model.Identity{
			Principal: pr.accountUUID,
			Extra:     identityExtra(pr),
		},
		Correlation: model.Correlation{
			PrevMessageID: pr.prevID,
			ThreadID:      threadID,
			RequestUUID:   pr.uuid,
			NumMessages:   pr.numMessages,
		},
		Input: model.Body{
			Source:      fileBody{path: pr.path, messages: pr.numMessages},
			ContentType: "application/json",
		},
	}
	if rm == nil {
		return rec
	}

	rec.Status = "200"
	rec.RequestID = rm.apiReqID
	rec.StopReason = rm.stopReason
	rec.Correlation.MessageID = rm.msgID
	rec.Input.TokenCount = rm.inputTokens
	rec.Input.CacheRead = rm.cacheRead
	rec.Input.CacheWrite = rm.cacheWrite
	rec.Output = model.Body{
		Source:      fileBody{path: rm.path, messages: -1},
		ContentType: "application/json",
		TokenCount:  rm.outputTokens,
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
