package vertexfetch

import (
	"context"
	"strings"
	"testing"
	"time"
)

const testEndpoint = "projects/my-proj/locations/us-central1/publishers/google/models/gemini-2.0-flash"

func TestBuildQuery(t *testing.T) {
	start := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2025, 3, 8, 0, 0, 0, 0, time.UTC)

	sql, params, err := buildQuery("my-proj", "vertex_logging", "request_response_logging", start, end)
	if err != nil {
		t.Fatalf("buildQuery error: %v", err)
	}

	if !strings.Contains(sql, "`my-proj.vertex_logging.request_response_logging`") {
		t.Errorf("query missing fully-qualified table reference:\n%s", sql)
	}
	if !strings.Contains(sql, "logging_time >= @start") || !strings.Contains(sql, "logging_time < @end") {
		t.Errorf("query missing parameterized time bounds:\n%s", sql)
	}
	if !strings.Contains(sql, "ORDER BY logging_time ASC") {
		t.Errorf("query missing ascending order:\n%s", sql)
	}
	if params["start"] != start || params["end"] != end {
		t.Errorf("params = %v, want start=%v end=%v", params, start, end)
	}
}

func TestBuildQueryUnboundedOmitsParams(t *testing.T) {
	sql, params, err := buildQuery("p", "d", "t", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("buildQuery error: %v", err)
	}
	if strings.Contains(sql, "@start") || strings.Contains(sql, "@end") {
		t.Errorf("unbounded query should not reference time params:\n%s", sql)
	}
	if len(params) != 0 {
		t.Errorf("params = %v, want empty", params)
	}
}

func TestBuildQueryRejectsInjection(t *testing.T) {
	cases := []struct{ project, dataset, table string }{
		{"my-proj", "vertex_logging", "t`; DROP TABLE x; --"},
		{"my proj", "d", "t"},
		{"p", "d'", "t"},
		{"", "d", "t"},
	}
	for _, c := range cases {
		if _, _, err := buildQuery(c.project, c.dataset, c.table, time.Time{}, time.Time{}); err == nil {
			t.Errorf("buildQuery(%q,%q,%q) = nil error, want rejection", c.project, c.dataset, c.table)
		}
	}
}

func TestBuildQueryAcceptsQualifiedProject(t *testing.T) {
	// Project ids can be fully-qualified (domain:project) and contain hyphens.
	if _, _, err := buildQuery("example.com:my-proj", "ds_1", "request_response_logging", time.Time{}, time.Time{}); err != nil {
		t.Errorf("buildQuery rejected a valid qualified project: %v", err)
	}
}

func TestRecordFromRowGemini(t *testing.T) {
	ts := time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC)
	row := vertexRow{
		Endpoint:        testEndpoint,
		DeployedModelID: "1234567890",
		LoggingTime:     ts,
		RequestID:       "42",
		RequestPayload:  []string{`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`},
		ResponsePayload: []string{`{"candidates":[{"content":{"role":"model","parts":[{"text":"hello"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":3,"cachedContentTokenCount":8}}`},
	}

	rec, ok := recordFromRow(row)
	if !ok {
		t.Fatal("recordFromRow returned ok=false")
	}
	if rec.Provider != "vertex" {
		t.Errorf("Provider = %q, want vertex", rec.Provider)
	}
	if rec.ModelID != "gemini-2.0-flash" {
		t.Errorf("ModelID = %q, want gemini-2.0-flash (from endpoint path)", rec.ModelID)
	}
	if rec.Operation != "generateContent" {
		t.Errorf("Operation = %q, want generateContent", rec.Operation)
	}
	if rec.RequestID != "42" {
		t.Errorf("RequestID = %q, want 42", rec.RequestID)
	}
	if rec.Identity.Principal != "" {
		t.Errorf("Identity.Principal = %q, want empty (schema has no per-row identity)", rec.Identity.Principal)
	}
	if rec.Identity.Extra["project"] != "my-proj" || rec.Identity.Extra["location"] != "us-central1" {
		t.Errorf("Identity.Extra = %v, want project/location populated", rec.Identity.Extra)
	}
	if rec.Input.TokenCount != 11 || rec.Output.TokenCount != 3 || rec.Input.CacheRead != 8 {
		t.Errorf("tokens: in=%d out=%d cacheRead=%d, want 11/3/8", rec.Input.TokenCount, rec.Output.TokenCount, rec.Input.CacheRead)
	}
	if rec.Input.CacheWrite != 0 {
		t.Errorf("CacheWrite = %d, want 0 (Gemini has no cache-write counter)", rec.Input.CacheWrite)
	}
	if rec.ErrorCode != "" {
		t.Errorf("ErrorCode = %q, want empty", rec.ErrorCode)
	}
}

func TestRecordFromRowSynthesizesRequestID(t *testing.T) {
	row := vertexRow{
		Endpoint:       testEndpoint,
		LoggingTime:    time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC),
		RequestID:      "0", // NUMERIC zero / absent
		RequestPayload: []string{`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`},
	}
	rec, ok := recordFromRow(row)
	if !ok {
		t.Fatal("ok=false")
	}
	if rec.RequestID == "" || rec.RequestID == "0" {
		t.Errorf("RequestID = %q, want a synthesized id", rec.RequestID)
	}
	if !strings.HasPrefix(rec.RequestID, "gemini-2.0-flash-") {
		t.Errorf("synthesized RequestID = %q, want model-prefixed", rec.RequestID)
	}
}

func TestRecordFromRowStreamingResponse(t *testing.T) {
	// Three streamed chunks; usage only in the final chunk. combineResponsePayload
	// wraps them in a JSON array and operation should be streamGenerateContent.
	row := vertexRow{
		Endpoint:       testEndpoint,
		LoggingTime:    time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC),
		RequestID:      "7",
		RequestPayload: []string{`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`},
		ResponsePayload: []string{
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hel"}]}}]}`,
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"lo"}]}}]}`,
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"!"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2}}`,
		},
	}
	rec, ok := recordFromRow(row)
	if !ok {
		t.Fatal("ok=false")
	}
	if rec.Operation != "streamGenerateContent" {
		t.Errorf("Operation = %q, want streamGenerateContent", rec.Operation)
	}
	if rec.Input.TokenCount != 5 || rec.Output.TokenCount != 2 {
		t.Errorf("tokens from final chunk: in=%d out=%d, want 5/2", rec.Input.TokenCount, rec.Output.TokenCount)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(rec.Output.JSON)), "[") {
		t.Errorf("streamed Output.JSON should be a JSON array, got: %s", rec.Output.JSON)
	}
}

func TestRecordFromRowErrorEnvelope(t *testing.T) {
	row := vertexRow{
		Endpoint:        testEndpoint,
		LoggingTime:     time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC),
		RequestID:       "9",
		RequestPayload:  []string{`{"contents":[{"role":"user","parts":[{"text":"x"}]}]}`},
		ResponsePayload: []string{`{"error":{"code":429,"message":"quota","status":"RESOURCE_EXHAUSTED"}}`},
	}
	rec, ok := recordFromRow(row)
	if !ok {
		t.Fatal("ok=false")
	}
	if rec.ErrorCode != "RESOURCE_EXHAUSTED" {
		t.Errorf("ErrorCode = %q, want RESOURCE_EXHAUSTED", rec.ErrorCode)
	}
}

func TestRecordFromRowFallsBackToDeployedModelID(t *testing.T) {
	// When the endpoint has no models/ segment, fall back to deployed_model_id.
	row := vertexRow{
		Endpoint:        "projects/p/locations/us-central1/endpoints/555",
		DeployedModelID: "my-tuned-model",
		LoggingTime:     time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC),
		RequestID:       "1",
		RequestPayload:  []string{`{"contents":[{"role":"user","parts":[{"text":"x"}]}]}`},
	}
	rec, _ := recordFromRow(row)
	if rec.ModelID != "my-tuned-model" {
		t.Errorf("ModelID = %q, want fallback to deployed_model_id", rec.ModelID)
	}
}

func TestRecordsFromRowsCountsMissingUsage(t *testing.T) {
	rows := []vertexRow{
		{ // has usage
			Endpoint: testEndpoint, LoggingTime: time.Now().UTC(), RequestID: "1",
			RequestPayload:  []string{`{"contents":[{"role":"user","parts":[{"text":"a"}]}]}`},
			ResponsePayload: []string{`{"candidates":[{"content":{"parts":[{"text":"b"}]}}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1}}`},
		},
		{ // response but no usage
			Endpoint: testEndpoint, LoggingTime: time.Now().UTC(), RequestID: "2",
			RequestPayload:  []string{`{"contents":[{"role":"user","parts":[{"text":"a"}]}]}`},
			ResponsePayload: []string{`{"candidates":[{"content":{"parts":[{"text":"b"}]}}]}`},
		},
		{ // empty row, dropped
			LoggingTime: time.Time{},
		},
	}
	records, missing := recordsFromRows(rows)
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2 (empty row dropped)", len(records))
	}
	if missing != 1 {
		t.Errorf("missingUsage = %d, want 1", missing)
	}
}

// fakeRunner is an injectable queryRunner that records the SQL/params it was
// called with and returns canned rows.
type fakeRunner struct {
	rows     []vertexRow
	gotSQL   string
	gotParam map[string]any
}

func (f *fakeRunner) run(_ context.Context, sql string, params map[string]any) ([]vertexRow, error) {
	f.gotSQL = sql
	f.gotParam = params
	return f.rows, nil
}

func TestFetchUsesQueryAndMapsRows(t *testing.T) {
	fr := &fakeRunner{rows: []vertexRow{{
		Endpoint:        testEndpoint,
		LoggingTime:     time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC),
		RequestID:       "1",
		RequestPayload:  []string{`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`},
		ResponsePayload: []string{`{"candidates":[{"content":{"parts":[{"text":"hello"}]}}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":2}}`},
	}}}
	f := &Fetcher{runner: fr, project: "p", dataset: "d", table: "t"}

	start := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)
	recs, err := f.Fetch(context.Background(), start, time.Time{})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	if recs[0].ModelID != "gemini-2.0-flash" {
		t.Errorf("ModelID = %q, want gemini-2.0-flash", recs[0].ModelID)
	}
	if !strings.Contains(fr.gotSQL, "`p.d.t`") {
		t.Errorf("Fetch did not build the expected table reference: %s", fr.gotSQL)
	}
	if fr.gotParam["start"] != start {
		t.Errorf("Fetch start param = %v, want %v", fr.gotParam["start"], start)
	}
}

func TestFetchRejectsBadIdentifiers(t *testing.T) {
	f := &Fetcher{runner: &fakeRunner{}, project: "p", dataset: "d", table: "bad`name"}
	if _, err := f.Fetch(context.Background(), time.Time{}, time.Time{}); err == nil {
		t.Error("Fetch with malicious table = nil error, want rejection before query runs")
	}
}
