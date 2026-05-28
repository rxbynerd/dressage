package azurefetch

import (
	"context"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/monitor/query/azlogs"
)

// fakeLogsClient implements the logsClient seam so Fetch can be exercised
// without a real Log Analytics workspace.
type fakeLogsClient struct {
	resp        azlogs.QueryWorkspaceResponse
	gotQuery    string
	gotTimespan *azlogs.TimeInterval
}

func (f *fakeLogsClient) QueryWorkspace(ctx context.Context, workspaceID string, body azlogs.QueryBody, opts *azlogs.QueryWorkspaceOptions) (azlogs.QueryWorkspaceResponse, error) {
	if body.Query != nil {
		f.gotQuery = *body.Query
	}
	f.gotTimespan = body.Timespan
	return f.resp, nil
}

func TestFetchDecodesThroughClient(t *testing.T) {
	props := `{"modelDeploymentName":"gpt-4o","callerObjectId":"oid","responseBody":"{\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}"}`
	fake := &fakeLogsClient{
		resp: azlogs.QueryWorkspaceResponse{
			QueryResults: azlogs.QueryResults{
				Tables: []azlogs.Table{{
					Columns: stdColumns(),
					Rows: []azlogs.Row{
						{
							"2025-03-01T09:00:00Z", "ChatCompletions_Create", float64(100),
							"200", "Success", "corr-1", "/subscriptions/s/resourceGroups/r/x",
							"10.0.0.1", "RequestResponse", "r", props,
						},
					},
				}},
			},
		},
	}

	f := &Fetcher{client: fake, workspaceID: "ws-guid"}

	start := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2025, 3, 2, 0, 0, 0, 0, time.UTC)
	records, err := f.Fetch(context.Background(), start, end)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	if records[0].ModelID != "gpt-4o" {
		t.Errorf("ModelID = %q, want gpt-4o", records[0].ModelID)
	}
	if records[0].Input.TokenCount != 5 {
		t.Errorf("Input.TokenCount = %d, want 5", records[0].Input.TokenCount)
	}

	// Timespan must be set when both bounds are present.
	if fake.gotTimespan == nil {
		t.Error("expected Timespan to be set when both start and end are provided")
	}
}

func TestFetchNoTimespanWhenUnbounded(t *testing.T) {
	fake := &fakeLogsClient{
		resp: azlogs.QueryWorkspaceResponse{
			QueryResults: azlogs.QueryResults{Tables: nil},
		},
	}
	f := &Fetcher{client: fake, workspaceID: "ws-guid"}

	if _, err := f.Fetch(context.Background(), time.Time{}, time.Time{}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if fake.gotTimespan != nil {
		t.Error("expected nil Timespan when start and end are zero")
	}
}
