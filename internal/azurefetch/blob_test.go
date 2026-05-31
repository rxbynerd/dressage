package azurefetch

import (
	"context"
	"strings"
	"testing"
	"time"
)

// fakeBlobLister is a test double for the blobLister seam.
type fakeBlobLister struct {
	blobs       map[string][]byte // name -> content
	gotPrefix   string
	listErr     error
	downloadErr error
}

func (f *fakeBlobLister) listBlobNames(ctx context.Context, prefix string) ([]string, error) {
	f.gotPrefix = prefix
	if f.listErr != nil {
		return nil, f.listErr
	}
	var names []string
	for n := range f.blobs {
		names = append(names, n)
	}
	return names, nil
}

func (f *fakeBlobLister) downloadBlob(ctx context.Context, name string) ([]byte, error) {
	if f.downloadErr != nil {
		return nil, f.downloadErr
	}
	return f.blobs[name], nil
}

// newTestBlobFetcher builds a BlobFetcher wired to the provided fake lister.
func newTestBlobFetcher(lister blobLister) *BlobFetcher {
	return &BlobFetcher{lister: lister}
}

const (
	resourceID = "/SUBSCRIPTIONS/SUB-1/RESOURCEGROUPS/RG/PROVIDERS/MICROSOFT.COGNITIVESERVICES/ACCOUNTS/myacct"
	blobPath   = "resourceId=" + resourceID + "/y=2026/m=01/d=15/h=10/m=00/PT1H.json"
)

func recordLine(ts string) string {
	// properties carries requestBody/responseBody as JSON-encoded strings, the
	// representation Azure diagnostic logging uses (matching properties_s).
	return `{"time":"` + ts + `","resourceId":"` + resourceID + `","operationName":"ChatCompletions_Create",` +
		`"category":"RequestResponse","resultType":"Success","resultSignature":"200","durationMs":420,` +
		`"callerIpAddress":"1.2.3.4","properties":{` +
		`"requestBody":"{\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}",` +
		`"responseBody":"{\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}",` +
		`"modelDeploymentName":"gpt-4o-deploy","modelName":"gpt-4o"}}`
}

func TestBlobFetchParsesAndSorts(t *testing.T) {
	// Two records in one blob, written out of order to prove sorting.
	content := []byte(recordLine("2026-01-15T10:45:00Z") + "\n" + recordLine("2026-01-15T10:30:00.1234567Z") + "\n")
	lister := &fakeBlobLister{blobs: map[string][]byte{blobPath: content}}
	f := newTestBlobFetcher(lister)

	records, err := f.Fetch(context.Background(), time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("want 2 records, got %d", len(records))
	}
	if !records[0].Timestamp.Before(records[1].Timestamp) {
		t.Errorf("records not sorted ascending: %v then %v", records[0].Timestamp, records[1].Timestamp)
	}

	r := records[0]
	if r.Provider != "azure" {
		t.Errorf("Provider = %q, want azure", r.Provider)
	}
	if r.ModelID != "gpt-4o-deploy" {
		t.Errorf("ModelID = %q, want gpt-4o-deploy (deployment preferred)", r.ModelID)
	}
	if r.Operation != "ChatCompletions_Create" {
		t.Errorf("Operation = %q", r.Operation)
	}
	if r.Status != "200" {
		t.Errorf("Status = %q, want 200", r.Status)
	}
	if r.ErrorCode != "" {
		t.Errorf("ErrorCode = %q, want empty for Success", r.ErrorCode)
	}
	if r.LatencyMs != 420 {
		t.Errorf("LatencyMs = %d, want 420", r.LatencyMs)
	}
	if !strings.Contains(string(r.Input.JSON), `"messages"`) {
		t.Errorf("Input.JSON missing messages: %s", r.Input.JSON)
	}
	if r.Input.TokenCount != 10 {
		t.Errorf("Input.TokenCount = %d, want 10", r.Input.TokenCount)
	}
	if r.Output.TokenCount != 5 {
		t.Errorf("Output.TokenCount = %d, want 5", r.Output.TokenCount)
	}
	if r.Identity.Extra["subscription"] != "SUB-1" {
		t.Errorf("subscription = %q, want SUB-1", r.Identity.Extra["subscription"])
	}
	if r.Identity.Extra["resourceGroup"] != "RG" {
		t.Errorf("resourceGroup = %q, want RG", r.Identity.Extra["resourceGroup"])
	}
	if r.Identity.Extra["callerIp"] != "1.2.3.4" {
		t.Errorf("callerIp = %q, want 1.2.3.4", r.Identity.Extra["callerIp"])
	}
}

func TestBlobFetchSkipsNonJSONAndOutOfRangeBlobs(t *testing.T) {
	lister := &fakeBlobLister{blobs: map[string][]byte{
		"some/path/_marker": []byte("ignore"), // not .json
		blobPath:            []byte(recordLine("2026-01-15T10:30:00Z")),
	}}
	f := newTestBlobFetcher(lister)

	// Window entirely after the blob's hour -> path prefilter drops it.
	start := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	records, err := f.Fetch(context.Background(), start, time.Time{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("want 0 records, got %d", len(records))
	}
}

func TestBlobFetchFiltersByRecordTime(t *testing.T) {
	// Two records in the same hour blob; window selects only the later one.
	content := []byte(recordLine("2026-01-15T10:05:00Z") + "\n" + recordLine("2026-01-15T10:50:00Z") + "\n")
	lister := &fakeBlobLister{blobs: map[string][]byte{blobPath: content}}
	f := newTestBlobFetcher(lister)

	start := time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC)
	end := time.Date(2026, 1, 15, 11, 0, 0, 0, time.UTC)
	records, err := f.Fetch(context.Background(), start, end)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("want 1 record in window, got %d", len(records))
	}
	if got := records[0].Timestamp.Minute(); got != 50 {
		t.Errorf("selected record minute = %d, want 50", got)
	}
}

func TestRecordsFromResourceLogsSkipsBadLines(t *testing.T) {
	content := []byte(strings.Join([]string{
		"",                            // blank
		"{ not json",                  // malformed
		`{"time":"","properties":{}}`, // no usable timestamp
		`{"time":"2026-01-15T10:00:00Z","category":"Audit","properties":{}}`, // wrong category
		recordLine("2026-01-15T10:30:00Z"),                                   // good
	}, "\n"))

	records, err := recordsFromResourceLogs(content, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("recordsFromResourceLogs: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("want 1 record, got %d", len(records))
	}
}

func TestBlobHour(t *testing.T) {
	hour, ok := blobHour(blobPath)
	if !ok {
		t.Fatal("blobHour failed to parse")
	}
	want := time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)
	if !hour.Equal(want) {
		// Guards against the trailing m=00 minute segment clobbering the month.
		t.Errorf("blobHour = %v, want %v", hour, want)
	}
	if _, ok := blobHour("no/date/segments.json"); ok {
		t.Error("expected parse failure for path without y=/m=/d=/h=")
	}
}

func TestBlobHourInRange(t *testing.T) {
	cases := []struct {
		name       string
		start, end time.Time
		want       bool
	}{
		{"unbounded", time.Time{}, time.Time{}, true},
		{"within", time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 16, 0, 0, 0, 0, time.UTC), true},
		{"start mid-hour keeps blob", time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC), time.Time{}, true},
		{"before window", time.Date(2026, 1, 15, 11, 0, 0, 0, time.UTC), time.Time{}, false},
		{"end at hour start excludes", time.Time{}, time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC), false},
		{"unparseable kept", time.Date(2026, 1, 15, 11, 0, 0, 0, time.UTC), time.Time{}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			name := blobPath
			if c.name == "unparseable kept" {
				name = "garbage.json"
			}
			if got := blobHourInRange(name, c.start, c.end); got != c.want {
				t.Errorf("blobHourInRange = %v, want %v", got, c.want)
			}
		})
	}
}
