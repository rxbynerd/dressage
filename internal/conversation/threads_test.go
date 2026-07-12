package conversation

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rxbynerd/dressage/internal/model"
)

func threadRecord(ts time.Time, threadID, input, output string) model.Record {
	return model.Record{
		Provider:    "claude",
		Timestamp:   ts,
		Correlation: model.Correlation{ThreadID: threadID},
		Input:       model.Body{JSON: json.RawMessage(input)},
		Output:      model.Body{JSON: json.RawMessage(output)},
	}
}

func TestReconstructThreads(t *testing.T) {
	base := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	mainIn1 := `{"messages":[{"role":"user","content":"main task"}]}`
	mainOut1 := `{"id":"msg_m1","role":"assistant","content":[{"type":"text","text":"spawning subagent"}],"stop_reason":"end_turn"}`
	subIn := `{"messages":[{"role":"user","content":"explore the codebase"}]}`
	subOut := `{"id":"msg_s1","role":"assistant","content":[{"type":"text","text":"sub result"}],"stop_reason":"end_turn"}`
	mainIn2 := `{"messages":[{"role":"user","content":"main task"},{"role":"assistant","content":"spawning subagent"},{"role":"user","content":"continue"}]}`
	mainOut2 := `{"id":"msg_m2","role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"}`

	records := []model.Record{
		threadRecord(base, "main-root", mainIn1, mainOut1),
		threadRecord(base.Add(5*time.Second), "sub-root", subIn, subOut),
		threadRecord(base.Add(10*time.Second), "main-root", mainIn2, mainOut2),
	}

	detail, sidechains := ReconstructThreads(records)
	if detail == nil {
		t.Fatal("expected main-thread detail")
	}
	// Main thread: 3 history messages from the fullest request + the final
	// assistant reply — and no contamination from the subagent's transcript.
	if len(detail.Turns) != 4 {
		t.Fatalf("main turns = %d, want 4", len(detail.Turns))
	}
	if !detail.Turns[0].HasText() || detail.Turns[0].Blocks[0].Text != "main task" {
		t.Errorf("main turn[0] = %+v, want the main task prompt", detail.Turns[0])
	}

	if len(sidechains) != 1 {
		t.Fatalf("sidechains = %d, want 1", len(sidechains))
	}
	sc := sidechains[0]
	if sc.ID != "sub-root" {
		t.Errorf("sidechain id = %q, want sub-root", sc.ID)
	}
	if sc.Detail == nil || len(sc.Detail.Turns) != 2 {
		t.Fatalf("sidechain detail = %+v, want 2 turns", sc.Detail)
	}
	if sc.Detail.Turns[0].Blocks[0].Text != "explore the codebase" {
		t.Errorf("sidechain turn[0] = %+v, want the subagent task prompt", sc.Detail.Turns[0])
	}
}

// TestReconstructThreadsSingleThread confirms records without thread ids (or
// all in one thread) behave exactly like Reconstruct: one detail, no
// sidechains.
func TestReconstructThreadsSingleThread(t *testing.T) {
	base := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	in := `{"messages":[{"role":"user","content":"hi"}]}`
	out := `{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn"}`

	for _, threadID := range []string{"", "only-thread"} {
		detail, sidechains := ReconstructThreads([]model.Record{threadRecord(base, threadID, in, out)})
		if detail == nil || len(detail.Turns) != 2 {
			t.Errorf("threadID=%q: detail = %+v, want 2 turns", threadID, detail)
		}
		if sidechains != nil {
			t.Errorf("threadID=%q: sidechains = %+v, want none", threadID, sidechains)
		}
	}
}
