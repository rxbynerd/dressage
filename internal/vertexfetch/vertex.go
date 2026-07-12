// Package vertexfetch retrieves Google Vertex AI request/response invocation
// logs from a BigQuery dataset and normalizes them into provider-neutral
// model.Record values.
//
// Vertex AI's "request-response logging" feature writes one BigQuery row per
// invocation into a per-endpoint table. The canonical schema (verified against
// the Vertex online-prediction logging docs) is:
//
//	endpoint           STRING            -- full resource path of the model/endpoint
//	deployed_model_id  STRING            -- deployment id (numeric-ish), not the model name
//	logging_time       TIMESTAMP         -- when the row was written
//	request_id         NUMERIC           -- per-request id (cast to STRING by the query)
//	request_payload    STRING REPEATED   -- serialized request JSON (usually one element)
//	response_payload   STRING REPEATED   -- serialized response JSON (one element, or one per streamed chunk)
//
// Two facts drive the design here and are documented in docs/providers/vertex.md:
//
//   - The payloads are serialized JSON *strings* in a REPEATED column, not
//     nested records. Streaming responses land as multiple elements (one per
//     streamed GenerateContentResponse) which are aggregated at reconstruction
//     time; this package wraps a multi-element response as a JSON array so the
//     Gemini reconstructor can detect and aggregate it.
//   - The request-response logging schema carries NO per-row caller identity
//     (no principal email). Identity attribution would require a Cloud Audit Log
//     join, which is out of scope. model.Identity.Principal is therefore left
//     empty for Vertex; the project, location, and endpoint are recorded in
//     Identity.Extra instead.
package vertexfetch

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/rxbynerd/dressage/internal/model"
)

// vertexRow is one decoded BigQuery row. The bigquery struct tags let the
// production fetcher load rows directly via the RowIterator; the pure mapping
// functions below operate on this type so the full decode path is unit-testable
// without a BigQuery client.
type vertexRow struct {
	Endpoint        string    `bigquery:"endpoint"`
	DeployedModelID string    `bigquery:"deployed_model_id"`
	LoggingTime     time.Time `bigquery:"logging_time"`
	RequestID       string    `bigquery:"request_id"`
	RequestPayload  []string  `bigquery:"request_payload"`
	ResponsePayload []string  `bigquery:"response_payload"`
}

// identifierRE matches the BigQuery identifier characters Dressage allows for
// the project, dataset, and table names. BigQuery does not permit parameterizing
// table identifiers, so these flag values are interpolated directly into the SQL
// and MUST be validated to prevent injection. The set is deliberately strict:
// letters, digits, underscores, hyphens, and (for fully-qualified projects)
// dots and colons. Anything else is rejected by validateIdentifier.
var identifierRE = regexp.MustCompile(`^[A-Za-z0-9_\-.:]+$`)

// validateIdentifier rejects a project/dataset/table identifier that contains
// characters outside the safe set, guarding the interpolated table reference in
// buildQuery against SQL injection.
func validateIdentifier(kind, value string) error {
	if value == "" {
		return fmt.Errorf("%s must not be empty", kind)
	}
	if !identifierRE.MatchString(value) {
		return fmt.Errorf("%s %q contains invalid characters (allowed: letters, digits, _-.:)", kind, value)
	}
	return nil
}

// buildQuery assembles the parameterized BigQuery SQL that selects request-
// response logging rows in [start, end) ordered by logging_time. The table
// reference is interpolated (BigQuery cannot parameterize identifiers) after the
// project/dataset/table identifiers are validated; the time bounds are passed as
// query parameters. request_id is cast to STRING so callers need not deal with
// NUMERIC/*big.Rat decoding. A zero start or end omits that bound.
func buildQuery(project, dataset, table string, start, end time.Time) (sql string, params map[string]any, err error) {
	for _, id := range []struct{ kind, value string }{
		{"project", project}, {"dataset", dataset}, {"table", table},
	} {
		if err := validateIdentifier(id.kind, id.value); err != nil {
			return "", nil, err
		}
	}

	params = map[string]any{}

	// COALESCE the scalar string columns to '' so a NULL cell loads as the empty
	// string rather than failing the BigQuery struct loader (which errors when a
	// NULL is loaded into a non-pointer Go field).
	var b strings.Builder
	fmt.Fprintf(&b, "SELECT\n"+
		"  COALESCE(endpoint, '') AS endpoint,\n"+
		"  COALESCE(deployed_model_id, '') AS deployed_model_id,\n"+
		"  logging_time,\n"+
		"  COALESCE(CAST(request_id AS STRING), '') AS request_id,\n"+
		"  request_payload,\n"+
		"  response_payload\n"+
		"FROM `%s.%s.%s`\n"+
		"WHERE TRUE", project, dataset, table)

	if !start.IsZero() {
		b.WriteString("\n  AND logging_time >= @start")
		params["start"] = start.UTC()
	}
	if !end.IsZero() {
		b.WriteString("\n  AND logging_time < @end")
		params["end"] = end.UTC()
	}
	b.WriteString("\nORDER BY logging_time ASC")

	return b.String(), params, nil
}

// recordsFromRows maps decoded BigQuery rows to provider-neutral records. It
// also returns the number of records whose response carried no usageMetadata, so
// the caller can emit a single per-run warning (token totals will be understated
// for those rows) rather than logging once per row.
func recordsFromRows(rows []vertexRow) (records []model.Record, missingUsage int) {
	for _, row := range rows {
		rec, ok := recordFromRow(row)
		if !ok {
			continue
		}
		if rec.Output.Present() && rec.Input.TokenCount == 0 && rec.Output.TokenCount == 0 {
			missingUsage++
		}
		records = append(records, rec)
	}
	return records, missingUsage
}

// recordFromRow decodes a single BigQuery row into a model.Record. The bool
// result is false when the row carries no usable payload at all.
func recordFromRow(row vertexRow) (model.Record, bool) {
	requestBody := combineRequestPayload(row.RequestPayload)
	responseBody, streamed := combineResponsePayload(row.ResponsePayload)

	if row.LoggingTime.IsZero() && row.RequestID == "" && len(requestBody) == 0 && len(responseBody) == 0 {
		return model.Record{}, false
	}

	rec := model.Record{
		Provider:  "vertex",
		Timestamp: row.LoggingTime.UTC(),
		RequestID: row.RequestID,
		ModelID:   modelID(row),
		Operation: operation(streamed),
		Input:     model.Body{JSON: requestBody, ContentType: "application/json"},
		Output:    model.Body{JSON: responseBody, ContentType: "application/json"},
	}

	// The request-response logging schema has no caller principal; record the
	// project/location/endpoint as identity attributes instead. See package doc.
	rec.Identity = model.Identity{Extra: identityExtra(row.Endpoint)}

	// A NUMERIC request_id of 0 (or absent) is not a useful id; synthesize a
	// stable one from (timestamp, model) so the report can still key on it.
	if rec.RequestID == "" || rec.RequestID == "0" {
		rec.RequestID = synthesizeRequestID(rec.Timestamp, rec.ModelID)
	}

	// Token accounting lives in the response usageMetadata (the final chunk for
	// streamed responses). Gemini reports cache reads but has no cache-write
	// counter, so CacheWrite stays 0.
	if u := parseUsageMetadata(responseBody); u != nil {
		rec.Input.TokenCount = u.PromptTokenCount
		rec.Input.CacheRead = u.CachedContentTokenCount
		rec.Output.TokenCount = u.CandidatesTokenCount
	}

	rec.ErrorCode = deriveErrorCode(responseBody)

	return rec, true
}

// operation labels the Vertex operation. The logging schema has no operation
// column, so it is inferred: a multi-chunk response indicates streaming.
func operation(streamed bool) string {
	if streamed {
		return "streamGenerateContent"
	}
	return "generateContent"
}

// modelID resolves the canonical model name used for envelope dispatch (e.g.
// "gemini-2.0-flash", "claude-3-5-sonnet"). The publisher model name lives in
// the trailing models/ segment of the endpoint resource path; deployed_model_id
// is only a numeric deployment id and is used as a fallback.
func modelID(row vertexRow) string {
	if m := pathSegment(row.Endpoint, "models"); m != "" {
		return m
	}
	return row.DeployedModelID
}

// identityExtra extracts the project, location, and endpoint from the endpoint
// resource path of the form
// projects/{project}/locations/{location}/publishers/{pub}/models/{model}.
func identityExtra(endpoint string) map[string]string {
	if endpoint == "" {
		return nil
	}
	extra := map[string]string{"endpoint": endpoint}
	if p := pathSegment(endpoint, "projects"); p != "" {
		extra["project"] = p
	}
	if l := pathSegment(endpoint, "locations"); l != "" {
		extra["location"] = l
	}
	return extra
}

// pathSegment returns the path segment immediately following the first segment
// equal to key in a slash-delimited resource path, or "" if not present.
func pathSegment(path, key string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == key {
			return parts[i+1]
		}
	}
	return ""
}

// synthesizeRequestID builds a stable, human-readable id for rows whose
// request_id is absent or zero, derived from the timestamp and model.
func synthesizeRequestID(ts time.Time, modelID string) string {
	if modelID == "" {
		modelID = "vertex"
	}
	return fmt.Sprintf("%s-%s", modelID, ts.UTC().Format("20060102T150405.000000000Z"))
}

// combineRequestPayload reduces the REPEATED request_payload column to a single
// JSON body. A single element is returned as-is. Multiple elements are first
// tried as a split-across-elements payload (concatenation), and the
// concatenation is used when it parses as JSON; otherwise the first element that
// parses as JSON is used (falling back to the first element).
func combineRequestPayload(payload []string) json.RawMessage {
	switch len(payload) {
	case 0:
		return nil
	case 1:
		if payload[0] == "" {
			return nil
		}
		return json.RawMessage(payload[0])
	}

	if joined := strings.Join(payload, ""); json.Valid([]byte(joined)) {
		return json.RawMessage(joined)
	}
	for _, p := range payload {
		if p != "" && json.Valid([]byte(p)) {
			return json.RawMessage(p)
		}
	}
	return json.RawMessage(payload[0])
}

// combineResponsePayload reduces the REPEATED response_payload column to a
// single JSON body and reports whether the response was streamed (more than one
// chunk). A single element is returned as-is. Multiple elements are wrapped into
// a JSON array of the valid-JSON chunks, which the Gemini reconstructor detects
// and aggregates.
func combineResponsePayload(payload []string) (body json.RawMessage, streamed bool) {
	switch len(payload) {
	case 0:
		return nil, false
	case 1:
		if payload[0] == "" {
			return nil, false
		}
		return json.RawMessage(payload[0]), false
	}

	var chunks []string
	for _, p := range payload {
		if p != "" && json.Valid([]byte(p)) {
			chunks = append(chunks, p)
		}
	}
	if len(chunks) == 0 {
		return json.RawMessage(payload[0]), true
	}
	return json.RawMessage("[" + strings.Join(chunks, ",") + "]"), true
}

// usageMetadata mirrors the Gemini usageMetadata block (subset). promptTokenCount
// includes cached tokens; cachedContentTokenCount is the cache-read count. Gemini
// has no cache-write equivalent.
type usageMetadata struct {
	PromptTokenCount        int64 `json:"promptTokenCount"`
	CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
	CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
}

// parseUsageMetadata extracts usageMetadata from a response body that is either
// a single GenerateContentResponse object or a JSON array of streamed chunks.
// For a streamed array it returns the usageMetadata of the last chunk that
// carries one (Gemini reports cumulative usage in the final chunk). Returns nil
// when no usageMetadata is present.
func parseUsageMetadata(responseBody json.RawMessage) *usageMetadata {
	if len(responseBody) == 0 {
		return nil
	}

	// Streamed: array of chunks. Walk backwards to the last chunk with usage.
	var chunks []json.RawMessage
	if err := json.Unmarshal(responseBody, &chunks); err == nil {
		for i := len(chunks) - 1; i >= 0; i-- {
			if u := usageFromObject(chunks[i]); u != nil {
				return u
			}
		}
		return nil
	}

	return usageFromObject(responseBody)
}

func usageFromObject(obj json.RawMessage) *usageMetadata {
	var env struct {
		UsageMetadata *usageMetadata `json:"usageMetadata"`
	}
	if err := json.Unmarshal(obj, &env); err != nil {
		return nil
	}
	if env.UsageMetadata == nil {
		return nil
	}
	// An all-zero usageMetadata (e.g. an empty {} block) carries no information.
	if env.UsageMetadata.PromptTokenCount == 0 && env.UsageMetadata.CandidatesTokenCount == 0 && env.UsageMetadata.CachedContentTokenCount == 0 {
		return nil
	}
	return env.UsageMetadata
}

// deriveErrorCode marks an errored invocation from the Gemini error envelope
// ({"error":{"code":..,"status":".."}}), which can appear as a bare object or as
// the first element of a streamed array. It prefers the symbolic status, falling
// back to the numeric code.
func deriveErrorCode(responseBody json.RawMessage) string {
	if len(responseBody) == 0 {
		return ""
	}

	obj := responseBody
	var chunks []json.RawMessage
	if err := json.Unmarshal(responseBody, &chunks); err == nil {
		if len(chunks) == 0 {
			return ""
		}
		obj = chunks[0]
	}

	var env struct {
		Error *struct {
			Code   int    `json:"code"`
			Status string `json:"status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(obj, &env); err != nil || env.Error == nil {
		return ""
	}
	if env.Error.Status != "" {
		return env.Error.Status
	}
	if env.Error.Code != 0 {
		return fmt.Sprintf("HTTP %d", env.Error.Code)
	}
	return ""
}
