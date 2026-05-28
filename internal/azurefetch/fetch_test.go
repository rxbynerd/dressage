package azurefetch

import (
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/monitor/query/azlogs"
)

// strPtr is a helper for the *string column names in azlogs.Column.
func strPtr(s string) *string { return &s }

// stdColumns returns the column set projected by buildQuery, in order.
func stdColumns() []azlogs.Column {
	names := []string{
		colTimeGenerated, colOperationName, colDurationMs, colResultSignature,
		colResultType, colCorrelationID, colResourceID, colCallerIPAddress,
		colCategory, colResourceGroup, colProperties,
	}
	cols := make([]azlogs.Column, len(names))
	for i, n := range names {
		cols[i] = azlogs.Column{Name: strPtr(n)}
	}
	return cols
}

func TestRecordsFromTables(t *testing.T) {
	const resourceID = "/subscriptions/sub-guid-123/resourceGroups/my-rg/providers/Microsoft.CognitiveServices/accounts/my-aoai"

	// Row 1: a normal completion. properties_s is a stringified JSON blob whose
	// requestBody/responseBody fields are themselves stringified JSON (the
	// double-encoding seen in real Azure diagnostic logs).
	normalProps := `{` +
		`"modelDeploymentName":"gpt-4o-deploy",` +
		`"callerObjectId":"oid-abc",` +
		`"requestBody":"{\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}",` +
		`"responseBody":"{\"choices\":[{\"message\":{\"role\":\"assistant\",\"content\":\"hello\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":3,\"prompt_tokens_details\":{\"cached_tokens\":8}}}"` +
		`}`

	// Row 2: a content-filtered prompt → HTTP 400 with an error envelope, no
	// usable completion body.
	errorProps := `{` +
		`"modelDeploymentName":"gpt-4o-deploy",` +
		`"callerObjectId":"oid-xyz",` +
		`"requestBody":"{\"messages\":[{\"role\":\"user\",\"content\":\"bad\"}]}",` +
		`"responseBody":"{\"error\":{\"code\":\"content_filter\",\"message\":\"blocked\"}}"` +
		`}`

	tables := []azlogs.Table{{
		Name:    strPtr("PrimaryResult"),
		Columns: stdColumns(),
		Rows: []azlogs.Row{
			{
				"2025-03-01T09:00:00Z",   // TimeGenerated
				"ChatCompletions_Create", // OperationName
				float64(1500),            // DurationMs (longs arrive as float64)
				"200",                    // ResultSignature
				"Success",                // ResultType
				"corr-1",                 // CorrelationId
				resourceID,               // _ResourceId
				"10.0.0.1",               // CallerIPAddress
				"RequestResponse",        // Category
				"my-rg",                  // ResourceGroup
				normalProps,              // properties_s
			},
			{
				"2025-03-01T09:05:00Z",
				"ChatCompletions_Create",
				float64(50),
				"400",
				"ClientError",
				"corr-2",
				resourceID,
				"10.0.0.2",
				"RequestResponse",
				"my-rg",
				errorProps,
			},
		},
	}}

	records, err := recordsFromTables(tables)
	if err != nil {
		t.Fatalf("recordsFromTables: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2", len(records))
	}

	// --- Row 1: normal completion ---
	r0 := records[0]
	if r0.Provider != "azure" {
		t.Errorf("Provider = %q, want azure", r0.Provider)
	}
	wantTS := time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC)
	if !r0.Timestamp.Equal(wantTS) {
		t.Errorf("Timestamp = %v, want %v", r0.Timestamp, wantTS)
	}
	if r0.ModelID != "gpt-4o-deploy" {
		t.Errorf("ModelID = %q, want gpt-4o-deploy", r0.ModelID)
	}
	if r0.Operation != "ChatCompletions_Create" {
		t.Errorf("Operation = %q, want ChatCompletions_Create", r0.Operation)
	}
	if r0.Status != "200" {
		t.Errorf("Status = %q, want 200", r0.Status)
	}
	if r0.ErrorCode != "" {
		t.Errorf("ErrorCode = %q, want empty", r0.ErrorCode)
	}
	if r0.RequestID != "corr-1" {
		t.Errorf("RequestID = %q, want corr-1", r0.RequestID)
	}
	if r0.LatencyMs != 1500 {
		t.Errorf("LatencyMs = %d, want 1500", r0.LatencyMs)
	}
	if r0.Input.TokenCount != 11 {
		t.Errorf("Input.TokenCount = %d, want 11", r0.Input.TokenCount)
	}
	if r0.Output.TokenCount != 3 {
		t.Errorf("Output.TokenCount = %d, want 3", r0.Output.TokenCount)
	}
	if r0.Input.CacheRead != 8 {
		t.Errorf("Input.CacheRead = %d, want 8", r0.Input.CacheRead)
	}
	if r0.Input.CacheWrite != 0 {
		t.Errorf("Input.CacheWrite = %d, want 0", r0.Input.CacheWrite)
	}
	if r0.Identity.Principal != "oid-abc" {
		t.Errorf("Identity.Principal = %q, want oid-abc", r0.Identity.Principal)
	}
	if r0.Identity.Extra["subscription"] != "sub-guid-123" {
		t.Errorf("Identity.Extra[subscription] = %q, want sub-guid-123", r0.Identity.Extra["subscription"])
	}
	if r0.Identity.Extra["resourceGroup"] != "my-rg" {
		t.Errorf("Identity.Extra[resourceGroup] = %q, want my-rg", r0.Identity.Extra["resourceGroup"])
	}
	if r0.Identity.Extra["callerIp"] != "10.0.0.1" {
		t.Errorf("Identity.Extra[callerIp] = %q, want 10.0.0.1", r0.Identity.Extra["callerIp"])
	}
	// The double-encoded request body should be unwrapped into real JSON.
	if !strings.Contains(string(r0.Input.JSON), `"role":"user"`) {
		t.Errorf("Input.JSON did not unwrap: %s", r0.Input.JSON)
	}

	// --- Row 2: content-filter error ---
	r1 := records[1]
	if r1.Status != "400" {
		t.Errorf("Status = %q, want 400", r1.Status)
	}
	if r1.ErrorCode != "content_filter" {
		t.Errorf("ErrorCode = %q, want content_filter", r1.ErrorCode)
	}
	if r1.Identity.Principal != "oid-xyz" {
		t.Errorf("Identity.Principal = %q, want oid-xyz", r1.Identity.Principal)
	}
	// No usable token counts on an error row.
	if r1.Input.TokenCount != 0 || r1.Output.TokenCount != 0 {
		t.Errorf("error row token counts = (%d,%d), want (0,0)", r1.Input.TokenCount, r1.Output.TokenCount)
	}
}

func TestRecordsFromTablesHTTPErrorFallback(t *testing.T) {
	// A non-2xx status with no error.code in the body should synthesize
	// "HTTP <status>".
	props := `{"responseBody":"{\"detail\":\"server exploded\"}"}`
	tables := []azlogs.Table{{
		Columns: stdColumns(),
		Rows: []azlogs.Row{
			{
				"2025-03-01T10:00:00Z", "ChatCompletions_Create", float64(10),
				"503", "ServerError", "corr-err", "/subscriptions/s/resourceGroups/r/x",
				"10.0.0.9", "RequestResponse", "r", props,
			},
		},
	}}
	records, err := recordsFromTables(tables)
	if err != nil {
		t.Fatalf("recordsFromTables: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	if records[0].ErrorCode != "HTTP 503" {
		t.Errorf("ErrorCode = %q, want 'HTTP 503'", records[0].ErrorCode)
	}
}

func TestRecordsFromTablesNilCells(t *testing.T) {
	// NULL cells arrive as nil and must not panic; a row with nothing
	// identifying is dropped.
	tables := []azlogs.Table{{
		Columns: stdColumns(),
		Rows: []azlogs.Row{
			{nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil},
		},
	}}
	records, err := recordsFromTables(tables)
	if err != nil {
		t.Fatalf("recordsFromTables: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("records = %d, want 0 (empty row dropped)", len(records))
	}
}

func TestBuildQuery(t *testing.T) {
	t.Run("category filter always present", func(t *testing.T) {
		f := &Fetcher{}
		q := f.buildQuery(time.Time{}, time.Time{})
		if !strings.Contains(q, `Category == "RequestResponse"`) {
			t.Errorf("query missing category filter:\n%s", q)
		}
		if !strings.Contains(q, "AzureDiagnostics") {
			t.Errorf("query missing AzureDiagnostics table:\n%s", q)
		}
		if !strings.Contains(q, "sort by TimeGenerated asc") {
			t.Errorf("query missing sort clause:\n%s", q)
		}
		if !strings.Contains(q, "project") || !strings.Contains(q, colProperties) {
			t.Errorf("query missing project of properties_s:\n%s", q)
		}
		// No optional filters when unset.
		if strings.Contains(q, "_ResourceId contains") {
			t.Errorf("query should not contain resource filter:\n%s", q)
		}
		if strings.Contains(q, "SubscriptionId ==") {
			t.Errorf("query should not contain subscription filter:\n%s", q)
		}
	})

	t.Run("optional filters when set", func(t *testing.T) {
		f := &Fetcher{subscription: "sub-1", resource: "my-aoai"}
		q := f.buildQuery(time.Time{}, time.Time{})
		if !strings.Contains(q, `_ResourceId contains "my-aoai"`) {
			t.Errorf("query missing resource filter:\n%s", q)
		}
		if !strings.Contains(q, `SubscriptionId == "sub-1"`) {
			t.Errorf("query missing subscription filter:\n%s", q)
		}
	})

	t.Run("time bounds when set", func(t *testing.T) {
		f := &Fetcher{}
		start := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)
		end := time.Date(2025, 3, 2, 0, 0, 0, 0, time.UTC)
		q := f.buildQuery(start, end)
		if !strings.Contains(q, "TimeGenerated >= datetime(2025-03-01T00:00:00Z)") {
			t.Errorf("query missing start bound:\n%s", q)
		}
		if !strings.Contains(q, "TimeGenerated < datetime(2025-03-02T00:00:00Z)") {
			t.Errorf("query missing end bound:\n%s", q)
		}
	})
}
