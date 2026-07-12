package conversation

import (
	"sort"
	"time"

	"github.com/rxbynerd/dressage/internal/model"
)

// ReconstructThreads builds a conversation's main-thread detail plus one
// detail per sidechain, using the fetcher-assigned Correlation.ThreadID to
// separate the linear chains (main thread, each subagent) interleaved within
// one session. Records without thread ids — providers that don't populate
// correlation metadata — collapse to a single thread, i.e. plain Reconstruct
// behavior.
//
// The main thread is the one that starts earliest: a session's opening
// request necessarily precedes any subagent it spawns. Ties prefer the thread
// with more invocations, then the smaller id, for determinism. Sidechains are
// returned in start order; ones that fail to reconstruct are dropped. A
// conversation whose main thread cannot be reconstructed yields (nil, nil).
func ReconstructThreads(records []model.Record) (*model.ConversationDetail, []model.Thread) {
	groups := make(map[string][]model.Record)
	ids := make([]string, 0)
	for _, rec := range records {
		id := rec.Correlation.ThreadID
		if _, ok := groups[id]; !ok {
			ids = append(ids, id)
		}
		groups[id] = append(groups[id], rec)
	}
	if len(ids) <= 1 {
		return Reconstruct(records), nil
	}

	type thread struct {
		id    string
		recs  []model.Record
		start time.Time
	}
	threads := make([]thread, 0, len(ids))
	for _, id := range ids {
		recs := groups[id]
		start := recs[0].Timestamp
		for _, r := range recs[1:] {
			if r.Timestamp.Before(start) {
				start = r.Timestamp
			}
		}
		threads = append(threads, thread{id: id, recs: recs, start: start})
	}
	sort.Slice(threads, func(i, j int) bool {
		if !threads[i].start.Equal(threads[j].start) {
			return threads[i].start.Before(threads[j].start)
		}
		if len(threads[i].recs) != len(threads[j].recs) {
			return len(threads[i].recs) > len(threads[j].recs)
		}
		return threads[i].id < threads[j].id
	})

	main := Reconstruct(threads[0].recs)
	if main == nil {
		return nil, nil
	}
	var sidechains []model.Thread
	for _, th := range threads[1:] {
		if d := Reconstruct(th.recs); d != nil {
			sidechains = append(sidechains, model.Thread{ID: th.id, Detail: d})
		}
	}
	return main, sidechains
}
