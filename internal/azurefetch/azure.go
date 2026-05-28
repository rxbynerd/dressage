package azurefetch

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/monitor/query/azlogs"
	"github.com/rxbynerd/dressage/internal/model"
)

// Candidate field-name lists for digging values out of the Azure OpenAI
// diagnostic log's dynamic `properties_s` blob.
//
// IMPORTANT: Microsoft does NOT document the exact field names used inside the
// Azure OpenAI RequestResponse log's property bag for the request body, the
// response body, the model/deployment name, or the Entra caller identity. They
// are not reliably promoted to dedicated `*_s` columns; they live inside the
// stringified-JSON `properties_s` column (sometimes double-encoded). These
// candidate lists are therefore best-effort guesses that MUST be confirmed
// against a real workspace. If your rows use different keys, add them here.
// See docs/providers/azure.md ("How Dressage finds payloads") for context.
var (
	requestBodyKeys  = []string{"requestBody", "RequestBody", "request", "input"}
	responseBodyKeys = []string{"responseBody", "ResponseBody", "response", "output"}
	deploymentKeys   = []string{"modelDeploymentName", "DeploymentName", "modelName", "model"}
	callerOIDKeys    = []string{"callerObjectId", "objectId", "oid", "callerId", "identity"}
)

// Column names projected by the KQL query (see buildQuery). Kept here so the
// pure recordsFromTables decoder and the query builder stay in sync.
const (
	colTimeGenerated   = "TimeGenerated"
	colOperationName   = "OperationName"
	colDurationMs      = "DurationMs"
	colResultSignature = "ResultSignature"
	colResultType      = "ResultType"
	colCorrelationID   = "CorrelationId"
	colResourceID      = "_ResourceId"
	colCallerIPAddress = "CallerIPAddress"
	colCategory        = "Category"
	colResourceGroup   = "ResourceGroup"
	colProperties      = "properties_s"
)

// recordsFromTables converts the tabular result of a Log Analytics query into
// provider-neutral records. It is a PURE function (no SDK/network calls) so the
// full decode path is unit-testable with synthetic tables.
//
// Cell typing notes (per the Log Analytics query API): numbers — including
// longs — arrive as float64; strings/datetime/guid/dynamic arrive as string;
// NULL cells arrive as nil. Every cell is nil-checked before use and indices
// are resolved by column name, never hardcoded.
func recordsFromTables(tables []azlogs.Table) ([]model.Record, error) {
	var records []model.Record
	for _, table := range tables {
		idx := columnIndex(table.Columns)
		for _, row := range table.Rows {
			rec, ok := recordFromRow(row, idx)
			if !ok {
				continue
			}
			records = append(records, rec)
		}
	}
	return records, nil
}

// columnIndex builds a column-name → row-index map from a table's columns.
func columnIndex(columns []azlogs.Column) map[string]int {
	idx := make(map[string]int, len(columns))
	for i, c := range columns {
		if c.Name != nil {
			idx[*c.Name] = i
		}
	}
	return idx
}

// recordFromRow decodes a single Log Analytics row into a model.Record. The
// bool result is false when the row holds no usable cells at all.
func recordFromRow(row azlogs.Row, idx map[string]int) (model.Record, bool) {
	get := func(name string) any {
		i, ok := idx[name]
		if !ok || i < 0 || i >= len(row) {
			return nil
		}
		return row[i]
	}

	rec := model.Record{Provider: "azure"}

	rec.Timestamp = cellTime(get(colTimeGenerated))
	rec.Operation = cellString(get(colOperationName))
	rec.Status = cellString(get(colResultSignature))
	rec.RequestID = cellString(get(colCorrelationID))
	rec.LatencyMs = cellInt64(get(colDurationMs))

	resourceID := cellString(get(colResourceID))
	resultType := cellString(get(colResultType))
	callerIP := cellString(get(colCallerIPAddress))
	resourceGroup := cellString(get(colResourceGroup))

	// Dig request/response bodies, deployment, and caller identity out of the
	// dynamic property bag (which itself may be a stringified JSON object).
	props := parseProperties(cellString(get(colProperties)))

	requestBody := unwrapJSONString(firstJSON(props, requestBodyKeys...))
	responseBody := unwrapJSONString(firstJSON(props, responseBodyKeys...))
	deployment := firstString(props, deploymentKeys...)
	callerOID := firstString(props, callerOIDKeys...)

	rec.ModelID = deployment
	rec.Input = model.Body{JSON: requestBody, ContentType: "application/json"}
	rec.Output = model.Body{JSON: responseBody, ContentType: "application/json"}

	// Token accounting lives in the response usage block.
	if u := parseUsage(responseBody); u != nil {
		rec.Input.TokenCount = u.PromptTokens
		rec.Input.CacheRead = u.PromptTokensDetails.CachedTokens
		rec.Output.TokenCount = u.CompletionTokens
		// OpenAI has no cache-write counter; CacheWrite stays 0.
	}

	rec.ErrorCode = deriveErrorCode(responseBody, rec.Status)

	rec.Identity = model.Identity{
		Principal: callerOID,
		Extra:     identityExtra(resourceID, resourceGroup, callerIP),
	}

	// Stash the raw property bag (when present) for future per-provider needs.
	if len(props) > 0 {
		if extras, err := json.Marshal(props); err == nil {
			rec.ProviderExtras = extras
		}
	}

	_ = resultType // currently informational only; reserved for future use

	// A row with nothing identifying is not useful.
	if rec.Timestamp.IsZero() && rec.RequestID == "" && len(requestBody) == 0 && len(responseBody) == 0 {
		return model.Record{}, false
	}
	return rec, true
}

// identityExtra collects the provider-specific identity attributes, parsing the
// subscription and resource group out of the ARM `_ResourceId` when available.
func identityExtra(resourceID, resourceGroup, callerIP string) map[string]string {
	extra := map[string]string{}
	sub, rg := parseResourceID(resourceID)
	if sub != "" {
		extra["subscription"] = sub
	}
	if rg == "" {
		rg = resourceGroup
	}
	if rg != "" {
		extra["resourceGroup"] = rg
	}
	if callerIP != "" {
		extra["callerIp"] = callerIP
	}
	if len(extra) == 0 {
		return nil
	}
	return extra
}

// parseResourceID extracts the subscription GUID and resource group name from a
// full ARM resource id of the form
// /subscriptions/{sub}/resourceGroups/{rg}/providers/...
// (the _ResourceId column from Azure Monitor is upper-cased; match case-insensitively).
func parseResourceID(resourceID string) (subscription, resourceGroup string) {
	if resourceID == "" {
		return "", ""
	}
	parts := strings.Split(strings.Trim(resourceID, "/"), "/")
	for i := 0; i+1 < len(parts); i++ {
		switch strings.ToLower(parts[i]) {
		case "subscriptions":
			subscription = parts[i+1]
		case "resourcegroups":
			resourceGroup = parts[i+1]
		}
	}
	return subscription, resourceGroup
}

// deriveErrorCode marks errored invocations. It prefers the OpenAI error code
// embedded in the response body ({"error":{"code":"content_filter",...}}), and
// otherwise synthesizes "HTTP <status>" for any non-2xx status.
func deriveErrorCode(responseBody json.RawMessage, status string) string {
	if len(responseBody) > 0 {
		var env struct {
			Error *struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(responseBody, &env); err == nil && env.Error != nil && env.Error.Code != "" {
			return env.Error.Code
		}
	}
	if status != "" && !strings.HasPrefix(status, "2") {
		return "HTTP " + status
	}
	return ""
}

// usage mirrors the OpenAI Chat Completions `usage` object (subset).
type usage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// parseUsage extracts the usage block from a (non-streaming) response body.
// Streaming chunk arrays do not carry usage by default, so nil is returned and
// token counts fall back to whatever the diagnostic columns provided (none).
func parseUsage(responseBody json.RawMessage) *usage {
	if len(responseBody) == 0 {
		return nil
	}
	var env struct {
		Usage *usage `json:"usage"`
	}
	if err := json.Unmarshal(responseBody, &env); err != nil {
		return nil
	}
	return env.Usage
}

// parseProperties parses the stringified-JSON `properties_s` column into a map
// of raw-JSON values keyed by field name. Returns nil on empty/invalid input.
func parseProperties(s string) map[string]json.RawMessage {
	if s == "" {
		return nil
	}
	var props map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &props); err != nil {
		return nil
	}
	return props
}

// firstJSON returns the raw JSON value for the first matching key (matched
// case-insensitively against the candidate list).
func firstJSON(props map[string]json.RawMessage, keys ...string) json.RawMessage {
	if len(props) == 0 {
		return nil
	}
	for _, want := range keys {
		for k, v := range props {
			if strings.EqualFold(k, want) {
				return v
			}
		}
	}
	return nil
}

// firstString returns the string value for the first matching key. A value that
// is a JSON string is unquoted; any other JSON value is returned verbatim.
func firstString(props map[string]json.RawMessage, keys ...string) string {
	raw := firstJSON(props, keys...)
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

// unwrapJSONString handles the double-encoding seen in Azure diagnostic logs: a
// field value may be a JSON object OR a JSON string whose contents are
// themselves JSON. If raw decodes to a string, that string's bytes are treated
// as the real JSON; otherwise raw is returned unchanged.
func unwrapJSONString(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return json.RawMessage(s)
	}
	return raw
}

// cellString coerces a Log Analytics cell to a string. datetime/guid/dynamic
// cells arrive as strings already; nil cells yield "".
func cellString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}

// cellInt64 coerces a numeric Log Analytics cell to int64. Numbers (incl longs)
// arrive as float64; nil yields 0.
func cellInt64(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	case json.Number:
		n, _ := t.Int64()
		return n
	default:
		return 0
	}
}

// cellTime parses a datetime cell. Log Analytics returns datetimes as RFC3339
// strings; some clients surface them as time.Time directly.
func cellTime(v any) time.Time {
	switch t := v.(type) {
	case time.Time:
		return t.UTC()
	case string:
		if parsed, err := time.Parse(time.RFC3339Nano, t); err == nil {
			return parsed.UTC()
		}
		if parsed, err := time.Parse(time.RFC3339, t); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}
