// Package azurefetch: ingestion of Azure OpenAI RequestResponse diagnostic logs
// that have been exported to an Azure Storage account, as opposed to a Log
// Analytics workspace (see fetch.go). Both paths normalize into the same
// provider-neutral model.Record values and share the property-bag decoding in
// azure.go, so reports are identical regardless of which sink delivered them.
//
// When a Cognitive Services / Azure OpenAI resource has a diagnostic setting
// that ships the "RequestResponse" category to a storage account, Azure Monitor
// writes hourly append blobs into the container insights-logs-<category> (the
// category lower-cased) using the layout:
//
//	insights-logs-requestresponse/resourceId=/SUBSCRIPTIONS/<sub>/RESOURCEGROUPS/<rg>/
//	  PROVIDERS/MICROSOFT.COGNITIVESERVICES/ACCOUNTS/<acct>/
//	  y=<YYYY>/m=<MM>/d=<DD>/h=<HH>/m=00/PT1H.json
//
// Each PT1H.json blob is newline-delimited JSON, one Azure Monitor resource-log
// record per line. The record's nested `properties` object carries the same
// RequestResponse fields (requestBody, responseBody, model name, ...) that the
// Log Analytics path finds in properties_s.
//
// See https://learn.microsoft.com/en-us/azure/ai-services/diagnostic-logging
// and https://learn.microsoft.com/en-us/azure/azure-monitor/essentials/resource-logs-schema
package azurefetch

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/rxbynerd/dressage/internal/fetch"
	"github.com/rxbynerd/dressage/internal/model"
)

// DefaultContainer is the blob container Azure Monitor uses for the Azure OpenAI
// "RequestResponse" diagnostic category. Container names follow the convention
// insights-logs-<category> with the category lower-cased.
const DefaultContainer = "insights-logs-requestresponse"

// scannerMaxLine bounds a single NDJSON line. Request/response bodies can be
// large, so this mirrors the generous buffer used by the Bedrock S3 fetcher.
const scannerMaxLine = 10 * 1024 * 1024

// blobLister is the subset of blob operations BlobFetcher needs, captured as an
// interface so tests can supply a fake without a live storage account (mirrors
// the logsClient seam used by the Log Analytics Fetcher).
type blobLister interface {
	// listBlobNames returns every blob name under prefix (empty prefix lists all).
	listBlobNames(ctx context.Context, prefix string) ([]string, error)
	// downloadBlob returns the full bytes of a single blob.
	downloadBlob(ctx context.Context, name string) ([]byte, error)
}

// BlobFetcher reads Azure OpenAI diagnostic logs from a storage account and
// normalizes them into model.Record values.
type BlobFetcher struct {
	lister blobLister
}

// BlobFetcher implements the provider-neutral fetch.Fetcher interface.
var _ fetch.Fetcher = (*BlobFetcher)(nil)

// NewBlobFetcher constructs a BlobFetcher for the named storage account using
// the supplied credential. An empty container falls back to DefaultContainer.
// The credential needs at least the "Storage Blob Data Reader" role on the
// account.
func NewBlobFetcher(cred azcore.TokenCredential, account, container string) (*BlobFetcher, error) {
	if strings.TrimSpace(account) == "" {
		return nil, fmt.Errorf("storage account name is required")
	}
	if strings.TrimSpace(container) == "" {
		container = DefaultContainer
	}
	serviceURL := fmt.Sprintf("https://%s.blob.core.windows.net/", account)
	client, err := azblob.NewClient(serviceURL, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("creating Blob client: %w", err)
	}
	return &BlobFetcher{lister: &azblobLister{client: client, container: container}}, nil
}

// Fetch lists the container, downloads each PT1H.json blob whose hour overlaps
// [start, end), parses the NDJSON resource-log records, and normalizes them. The
// blob path provides a coarse hour-level prefilter; each record's own timestamp
// is then checked so partial hours at the window boundaries are trimmed exactly.
// A zero start/end means unbounded on that side.
func (f *BlobFetcher) Fetch(ctx context.Context, start, end time.Time) ([]model.Record, error) {
	names, err := f.lister.listBlobNames(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("listing diagnostic log blobs: %w", err)
	}

	var records []model.Record
	for _, name := range names {
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		if !blobHourInRange(name, start, end) {
			continue
		}
		data, err := f.lister.downloadBlob(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("downloading %s: %w", name, err)
		}
		recs, err := recordsFromResourceLogs(data, start, end)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", name, err)
		}
		records = append(records, recs...)
	}

	sort.Slice(records, func(i, j int) bool {
		return records[i].Timestamp.Before(records[j].Timestamp)
	})
	return records, nil
}

// resourceLogRecord is the subset of the Azure Monitor common resource-log
// schema we consume from storage-exported diagnostic logs. The RequestResponse
// payload lives in the nested `properties` object, decoded via decodeProperties.
type resourceLogRecord struct {
	Time            string          `json:"time"`
	ResourceID      string          `json:"resourceId"`
	OperationName   string          `json:"operationName"`
	Category        string          `json:"category"`
	ResultType      string          `json:"resultType"`
	ResultSignature string          `json:"resultSignature"`
	DurationMs      json.Number     `json:"durationMs"`
	CorrelationID   string          `json:"correlationId"`
	CallerIPAddress string          `json:"callerIpAddress"`
	Properties      json.RawMessage `json:"properties"`
}

// recordsFromResourceLogs parses newline-delimited resource-log records and
// normalizes the RequestResponse rows that fall within [start, end). Lines that
// fail to parse, lack a timestamp, or sit outside the window are skipped rather
// than failing the whole blob.
func recordsFromResourceLogs(data []byte, start, end time.Time) ([]model.Record, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), scannerMaxLine)

	var records []model.Record
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var raw resourceLogRecord
		if err := json.Unmarshal(line, &raw); err != nil {
			continue
		}
		// The container is already category-scoped, but guard defensively in
		// case multiple categories share a sink.
		if raw.Category != "" && !strings.EqualFold(raw.Category, "RequestResponse") {
			continue
		}
		rec, ok := recordFromResourceLog(raw)
		if !ok {
			continue
		}
		if !start.IsZero() && rec.Timestamp.Before(start) {
			continue
		}
		if !end.IsZero() && !rec.Timestamp.Before(end) {
			continue // end is exclusive: drop ts >= end
		}
		records = append(records, rec)
	}
	return records, scanner.Err()
}

// recordFromResourceLog maps a single resource-log record onto a model.Record.
// It reuses the property-bag digging, body/token mapping, error derivation, and
// identity helpers from azure.go so a storage-sourced record decodes identically
// to its Log Analytics counterpart. ok=false marks records with no usable
// timestamp. The property bag may carry requestBody/responseBody as nested JSON
// or as double-encoded JSON strings; unwrapJSONString normalizes both.
func recordFromResourceLog(raw resourceLogRecord) (model.Record, bool) {
	ts := cellTime(raw.Time)
	if ts.IsZero() {
		return model.Record{}, false
	}

	props := parseProperties(string(raw.Properties))

	requestBody := unwrapJSONString(firstJSON(props, requestBodyKeys...))
	responseBody := unwrapJSONString(firstJSON(props, responseBodyKeys...))
	deployment := firstString(props, deploymentKeys...)
	callerOID := firstString(props, callerOIDKeys...)

	rec := model.Record{
		Provider:  "azure",
		Timestamp: ts,
		Operation: raw.OperationName,
		Status:    raw.ResultSignature,
		RequestID: raw.CorrelationID,
		LatencyMs: cellInt64(raw.DurationMs),
		ModelID:   deployment,
		Input:     model.Body{JSON: requestBody, ContentType: "application/json"},
		Output:    model.Body{JSON: responseBody, ContentType: "application/json"},
	}

	// Token accounting lives in the response usage block.
	if u := parseUsage(responseBody); u != nil {
		rec.Input.TokenCount = u.PromptTokens
		rec.Input.CacheRead = u.PromptTokensDetails.CachedTokens
		rec.Output.TokenCount = u.CompletionTokens
	}

	rec.ErrorCode = deriveErrorCode(responseBody, rec.Status)

	rec.Identity = model.Identity{
		Principal: callerOID,
		Extra:     identityExtra(raw.ResourceID, "", raw.CallerIPAddress),
	}

	if len(props) > 0 {
		if extras, err := json.Marshal(props); err == nil {
			rec.ProviderExtras = extras
		}
	}

	return rec, true
}

// blobHourInRange reports whether an hourly blob overlaps [start, end) based on
// the y=/m=/d=/h= segments in its path. A blob covers [hour, hour+1h). When the
// path can't be parsed the blob is kept so logs are never silently dropped.
func blobHourInRange(name string, start, end time.Time) bool {
	if start.IsZero() && end.IsZero() {
		return true
	}
	hour, ok := blobHour(name)
	if !ok {
		return true
	}
	blobEnd := hour.Add(time.Hour)
	if !start.IsZero() && !blobEnd.After(start) {
		return false // blob ends at or before the window start
	}
	if !end.IsZero() && !hour.Before(end) {
		return false // blob starts at or after the (exclusive) window end
	}
	return true
}

// blobHour extracts the UTC hour from a .../y=YYYY/m=MM/d=DD/h=HH/m=00/... path.
// The trailing m=00 minute segment shares the "m=" prefix with the month, so
// only the first m= (which precedes the day segment) is taken as the month.
func blobHour(name string) (time.Time, bool) {
	var y, mo, d, h string
	sawDay := false
	for _, seg := range strings.Split(name, "/") {
		switch {
		case strings.HasPrefix(seg, "y="):
			y = seg[2:]
		case strings.HasPrefix(seg, "m="):
			if !sawDay && mo == "" {
				mo = seg[2:] // month precedes the day segment; ignore the minute trailer
			}
		case strings.HasPrefix(seg, "d="):
			d = seg[2:]
			sawDay = true
		case strings.HasPrefix(seg, "h="):
			h = seg[2:]
		}
	}
	if y == "" || mo == "" || d == "" || h == "" {
		return time.Time{}, false
	}
	year, e1 := strconv.Atoi(y)
	month, e2 := strconv.Atoi(mo)
	day, e3 := strconv.Atoi(d)
	hourN, e4 := strconv.Atoi(h)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || month < 1 || month > 12 {
		return time.Time{}, false
	}
	return time.Date(year, time.Month(month), day, hourN, 0, 0, 0, time.UTC), true
}

// azblobLister is the production blobLister backed by an *azblob.Client.
type azblobLister struct {
	client    *azblob.Client
	container string
}

func (l *azblobLister) listBlobNames(ctx context.Context, prefix string) ([]string, error) {
	var opts *azblob.ListBlobsFlatOptions
	if prefix != "" {
		opts = &azblob.ListBlobsFlatOptions{Prefix: &prefix}
	}
	pager := l.client.NewListBlobsFlatPager(l.container, opts)

	var names []string
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		if page.Segment == nil {
			continue
		}
		for _, b := range page.Segment.BlobItems {
			if b != nil && b.Name != nil {
				names = append(names, *b.Name)
			}
		}
	}
	return names, nil
}

func (l *azblobLister) downloadBlob(ctx context.Context, name string) ([]byte, error) {
	resp, err := l.client.DownloadStream(ctx, l.container, name, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}
