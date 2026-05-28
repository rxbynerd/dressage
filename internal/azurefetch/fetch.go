// Package azurefetch retrieves Azure OpenAI request/response invocation logs
// from an Azure Monitor Log Analytics workspace and normalizes them into
// provider-neutral model.Record values.
//
// Azure OpenAI (Cognitive Services) ships its RequestResponse diagnostic logs
// to the SHARED AzureDiagnostics table — there is no resource-specific table
// for it, and --export-to-resource-specific is a no-op for Cognitive Services.
// The request body, response body, model/deployment name, and Entra caller
// identity are NOT promoted to dedicated columns; they live inside the dynamic
// `properties_s` property bag, which is decoded in azure.go.
package azurefetch

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/monitor/query/azlogs"
	"github.com/rxbynerd/dressage/internal/fetch"
	"github.com/rxbynerd/dressage/internal/model"
)

// logsClient is the subset of *azlogs.Client used by Fetcher, enabling a fake
// to be injected in tests (mirrors the s3API seam in internal/s3fetch).
type logsClient interface {
	QueryWorkspace(ctx context.Context, workspaceID string, body azlogs.QueryBody, opts *azlogs.QueryWorkspaceOptions) (azlogs.QueryWorkspaceResponse, error)
}

// Fetcher queries a Log Analytics workspace for Azure OpenAI RequestResponse
// logs and normalizes them into model.Record values.
type Fetcher struct {
	client       logsClient
	workspaceID  string
	subscription string
	resource     string
}

// Fetcher implements the provider-neutral fetch.Fetcher interface.
var _ fetch.Fetcher = (*Fetcher)(nil)

// New constructs a Fetcher backed by a real azlogs.Client. workspaceID is the
// Log Analytics workspace GUID (Portal: workspace → Overview → Workspace ID).
// subscription and resource are optional narrowing filters applied inside the
// KQL query when non-empty.
func New(cred azcore.TokenCredential, workspaceID, subscription, resource string) (*Fetcher, error) {
	client, err := azlogs.NewClient(cred, nil)
	if err != nil {
		return nil, fmt.Errorf("creating Log Analytics client: %w", err)
	}
	return &Fetcher{
		client:       client,
		workspaceID:  workspaceID,
		subscription: subscription,
		resource:     resource,
	}, nil
}

// Fetch queries the workspace for RequestResponse rows in [start, end), then
// decodes the tabular result into provider-neutral records. A zero start/end
// means unbounded on that side; the Timespan is only set when both are present.
func (f *Fetcher) Fetch(ctx context.Context, start, end time.Time) ([]model.Record, error) {
	query := f.buildQuery(start, end)

	body := azlogs.QueryBody{Query: &query}
	if !start.IsZero() && !end.IsZero() {
		ti := azlogs.NewTimeInterval(start.UTC(), end.UTC())
		body.Timespan = &ti
	}

	resp, err := f.client.QueryWorkspace(ctx, f.workspaceID, body, nil)
	if err != nil {
		return nil, fmt.Errorf("querying Log Analytics workspace: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("Log Analytics query error: %s", resp.Error.Error())
	}

	return recordsFromTables(resp.Tables)
}

// buildQuery assembles the KQL query against AzureDiagnostics. It filters to the
// RequestResponse category, applies optional resource/subscription narrowing,
// projects the columns recordsFromTables expects (including the dynamic
// properties_s blob), and sorts by time ascending so downstream ordering is
// stable. The time window is also enforced server-side via QueryBody.Timespan
// when both bounds are set.
func (f *Fetcher) buildQuery(start, end time.Time) string {
	var b strings.Builder
	b.WriteString("AzureDiagnostics")
	b.WriteString("\n| where Category == \"RequestResponse\"")

	if f.resource != "" {
		fmt.Fprintf(&b, "\n| where _ResourceId contains %s", kqlString(f.resource))
	}
	if f.subscription != "" {
		fmt.Fprintf(&b, "\n| where SubscriptionId == %s", kqlString(f.subscription))
	}

	// Time bounds are also applied via Timespan; including them in the query is
	// harmless and keeps the query self-describing when Timespan is unset.
	if !start.IsZero() {
		fmt.Fprintf(&b, "\n| where TimeGenerated >= datetime(%s)", start.UTC().Format(time.RFC3339))
	}
	if !end.IsZero() {
		fmt.Fprintf(&b, "\n| where TimeGenerated < datetime(%s)", end.UTC().Format(time.RFC3339))
	}

	fmt.Fprintf(&b, "\n| project %s",
		strings.Join([]string{
			colTimeGenerated, colOperationName, colDurationMs, colResultSignature,
			colResultType, colCorrelationID, colResourceID, colCallerIPAddress,
			colCategory, colResourceGroup, colProperties,
		}, ", "))
	b.WriteString("\n| sort by TimeGenerated asc")

	return b.String()
}

// kqlString quotes and escapes a string literal for safe embedding in KQL.
func kqlString(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}
