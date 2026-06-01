package vertexfetch

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"cloud.google.com/go/auth/credentials"
	"cloud.google.com/go/bigquery"
	"github.com/rxbynerd/dressage/internal/fetch"
	"github.com/rxbynerd/dressage/internal/model"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// bigqueryScope is the OAuth scope requested when building credentials from an
// explicit service-account key file. Dressage only ever runs SELECT queries, so
// it requests the read-only scope to keep the blast radius of a leaked key
// confined to reads (no table/dataset mutation). The ADC path leaves scope
// selection to the token source / SDK defaults.
const bigqueryScope = "https://www.googleapis.com/auth/bigquery.readonly"

// queryRunner abstracts running a parameterized BigQuery query and decoding the
// result rows. The production implementation (bigqueryRunner) uses a
// *bigquery.Client and the RowIterator; tests inject a fake so the full Fetch
// path is exercisable without a live BigQuery dataset.
type queryRunner interface {
	run(ctx context.Context, sql string, params map[string]any) ([]vertexRow, error)
}

// Fetcher queries a Vertex AI request-response logging table in BigQuery and
// normalizes the rows into model.Record values.
type Fetcher struct {
	runner  queryRunner
	project string
	dataset string
	table   string
}

// Fetcher implements the provider-neutral fetch.Fetcher interface.
var _ fetch.Fetcher = (*Fetcher)(nil)

// New constructs a Fetcher backed by a real BigQuery client. project/dataset/
// table identify the request-response logging table (Vertex auto-creates a
// per-endpoint dataset named logging_<endpoint-display-name>_<endpoint-id> and a
// table named request_response_logging — see docs/providers/vertex.md). location
// is the BigQuery dataset location (e.g. "us-central1"); pass "" to let BigQuery
// infer it.
func New(client *bigquery.Client, project, dataset, table, location string) *Fetcher {
	return &Fetcher{
		runner:  &bigqueryRunner{client: client, location: location},
		project: project,
		dataset: dataset,
		table:   table,
	}
}

// NewClient builds a BigQuery client for the given billing project using
// Application Default Credentials, or an explicit service-account key file when
// credentialsFile is non-empty. ADC resolves, in order: GOOGLE_APPLICATION_
// CREDENTIALS, gcloud user credentials, and the attached workload identity when
// running inside GCP.
func NewClient(ctx context.Context, project, credentialsFile string) (*bigquery.Client, error) {
	// Validate the project up front (it is the billing project passed to the
	// SDK) so a malformed value fails with a clear Dressage error rather than an
	// opaque Google API response. buildQuery validates it again before it reaches
	// SQL; this is the defence-in-depth front door.
	if err := validateIdentifier("project", project); err != nil {
		return nil, err
	}

	var opts []option.ClientOption
	if credentialsFile != "" {
		// Build credentials from the key file via the cloud.google.com/go/auth
		// path; option.WithCredentialsFile/JSON are deprecated by the SDK.
		creds, err := credentials.DetectDefault(&credentials.DetectOptions{
			CredentialsFile: credentialsFile,
			Scopes:          []string{bigqueryScope},
		})
		if err != nil {
			return nil, fmt.Errorf("loading credentials from %q: %w", credentialsFile, err)
		}
		opts = append(opts, option.WithAuthCredentials(creds))
	}
	client, err := bigquery.NewClient(ctx, project, opts...)
	if err != nil {
		return nil, fmt.Errorf("creating BigQuery client: %w", err)
	}
	return client, nil
}

// Fetch runs the request-response logging query for [start, end), decodes the
// rows into provider-neutral records, and emits a single warning when any
// response lacked usageMetadata (its token totals are understated). A zero
// start/end means unbounded on that side.
func (f *Fetcher) Fetch(ctx context.Context, start, end time.Time) ([]model.Record, error) {
	sql, params, err := buildQuery(f.project, f.dataset, f.table, start, end)
	if err != nil {
		return nil, err
	}

	rows, err := f.runner.run(ctx, sql, params)
	if err != nil {
		return nil, err
	}

	records, missingUsage := recordsFromRows(rows)
	if missingUsage > 0 {
		log.Printf("vertex: %d response(s) had no usageMetadata; token totals for those invocations are understated "+
			"(commonly streamed rows logged without a final usage chunk)", missingUsage)
	}
	return records, nil
}

// bigqueryRunner is the production queryRunner: it executes the query via the
// BigQuery client and loads each row into a vertexRow through the RowIterator.
type bigqueryRunner struct {
	client   *bigquery.Client
	location string
}

func (r *bigqueryRunner) run(ctx context.Context, sql string, params map[string]any) ([]vertexRow, error) {
	q := r.client.Query(sql)
	if r.location != "" {
		q.Location = r.location
	}
	for name, val := range params {
		q.Parameters = append(q.Parameters, bigquery.QueryParameter{Name: name, Value: val})
	}

	it, err := q.Read(ctx)
	if err != nil {
		return nil, fmt.Errorf("running BigQuery query: %w", err)
	}

	var rows []vertexRow
	for {
		var row vertexRow
		err := it.Next(&row)
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading BigQuery row: %w", err)
		}
		rows = append(rows, row)
	}
	return rows, nil
}
