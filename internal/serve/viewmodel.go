package serve

import (
	"time"

	"github.com/rxbynerd/dressage/internal/ir"
)

// indexView is the data the index template renders. It is built entirely from
// the manifest — no conversation files are opened — by regrouping the flat,
// start-time-sorted conversation index into per-UTC-day buckets (the manifest
// does not persist day boundaries; the summary layer that produced it did the
// same bucketing). Per-day rollups are summed from the entries, and the header
// span is the actual min/max of the data (distinct from the requested
// date_range filter the manifest records under source).
type indexView struct {
	Title       string
	Provider    string
	GeneratedAt time.Time
	Command     string
	SpanStart   time.Time
	SpanEnd     time.Time
	HasSpan     bool
	RawBodies   string
	Totals      ir.ManifestTotals
	DayCount    int
	Days        []dayView
}

// dayView is one UTC day's card on the index: its date, summed counters, and
// the conversations that started that day (as links, not inlined content).
type dayView struct {
	Date            time.Time
	InvocationCount int
	InputTokens     int64
	OutputTokens    int64
	ErrorCount      int
	Conversations   []convLink
}

// convLink is a single conversation's index row: the URL-safe name to link to
// plus the manifest entry the template reads for its badges and counters.
type convLink struct {
	Name  string
	Entry ir.ManifestEntry
}

// buildIndexView regroups the manifest's flat conversation index into per-day
// cards and computes the header aggregates. The manifest is already sorted by
// (start_time, id), so days and their conversations come out in ascending
// order without an extra sort.
func buildIndexView(m *ir.Manifest) indexView {
	iv := indexView{
		Title:       m.Source.Provider + " — dressage",
		Provider:    m.Source.Provider,
		GeneratedAt: m.GeneratedAt,
		Command:     m.Source.Command,
		RawBodies:   m.RawBodies,
		Totals:      m.Totals,
	}
	if iv.Title == " — dressage" {
		iv.Title = "dressage"
	}

	byDay := make(map[string]int) // day key -> index into iv.Days
	for _, e := range m.Conversations {
		if iv.SpanStart.IsZero() || e.StartTime.Before(iv.SpanStart) {
			iv.SpanStart = e.StartTime
		}
		if e.EndTime.After(iv.SpanEnd) {
			iv.SpanEnd = e.EndTime
		}

		key := e.StartTime.UTC().Format("2006-01-02")
		idx, ok := byDay[key]
		if !ok {
			idx = len(iv.Days)
			byDay[key] = idx
			iv.Days = append(iv.Days, dayView{Date: e.StartTime.UTC().Truncate(24 * time.Hour)})
		}
		d := &iv.Days[idx]
		d.InvocationCount += e.InvocationCount
		d.InputTokens += e.InputTokens
		d.OutputTokens += e.OutputTokens
		d.ErrorCount += e.ErrorCount
		d.Conversations = append(d.Conversations, convLink{Name: e.Name(), Entry: e})
	}

	iv.HasSpan = !iv.SpanStart.IsZero()
	iv.DayCount = len(iv.Days)
	return iv
}

// convView is the data the conversation template renders: the loaded
// conversation IR plus the run's raw-bodies mode (so the raw-invocation section
// knows whether payloads are available to show).
type convView struct {
	Title          string
	Conv           *ir.ConversationIR
	Name           string
	BodiesEmbedded bool
}

// buildConvView wraps a loaded conversation for rendering.
func buildConvView(conv *ir.ConversationIR, name string, bodiesEmbedded bool) convView {
	title := conv.DisplayID
	if title == "" {
		title = conv.ID
	}
	return convView{
		Title:          title + " — dressage",
		Conv:           conv,
		Name:           name,
		BodiesEmbedded: bodiesEmbedded,
	}
}
