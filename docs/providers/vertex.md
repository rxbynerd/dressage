# Vertex AI / Gemini

Analyze [Google Vertex AI request-response logs](https://docs.cloud.google.com/vertex-ai/docs/predictions/online-prediction-logging)
that Vertex writes, per publisher model, into a BigQuery table — the closest
analogue to Bedrock's S3 invocation logs. Dressage reconstructs **Gemini**
conversations natively from the `contents[]`/`parts[]` envelope and reuses the
same per-day summaries and turn-by-turn HTML drill-down as the other providers.

Vertex's **request-response logging** writes one BigQuery row per invocation,
containing the full request JSON, the full response JSON, the model resource
name, and timestamps. Cloud Audit Logs also record Vertex invocations but
typically omit the request/response bodies, so they are insufficient for the
conversation drill-down and are not used.

> **Anthropic-on-Vertex (Claude) is not reconstructed here.** Claude models
> served through Vertex appear in the summary statistics (counts, tokens) but do
> not yet get the turn-by-turn drill-down; that is tracked as a follow-up in
> [#4](https://github.com/rxbynerd/dressage/issues/4). Non-Gemini, non-Anthropic
> Model Garden models (Llama, Mistral, …) are out of scope.

## Prerequisites

- Go 1.25+ (only to build from source).
- A GCP project with the **Vertex AI API** and the **BigQuery API** enabled.
- A Gemini model in use through Vertex (a publisher model or a dedicated
  endpoint), with **request-response logging enabled** (see below).
- Application Default Credentials resolvable by the Google SDK (a service
  account key, `gcloud auth application-default login`, or an attached workload
  identity) with the [required IAM](#required-iam) on the logging dataset.

## Enabling request-response logging

Dressage reads logs that already exist in BigQuery; it does not enable logging
for you. Request-response logging is configured **per publisher model** (or per
dedicated endpoint) and writes rows into a BigQuery table that Vertex
auto-creates.

> **Verify availability first.** Request-response logging for publisher models
> requires a **dedicated** or **Private Service Connect** endpoint, and payloads
> must be under 10 MB. Availability and the exact BigQuery schema have shifted
> over time — confirm against the
> [logging docs](https://docs.cloud.google.com/vertex-ai/docs/predictions/online-prediction-logging)
> before relying on it.

### Using the gcloud CLI

Enable logging on a Gemini publisher model, pointing it at a BigQuery dataset
you own (`bq://PROJECT.DATASET`):

```bash
gcloud ai model-monitoring publisher-models update \
  --publisher=google \
  --model=gemini-2.0-flash \
  --location=us-central1 \
  --request-response-logging-table=bq://my-gcp-project.vertex_logging \
  --request-response-logging-rate=1.0
```

`--request-response-logging-rate` is the sampling fraction (`1.0` logs every
request). The exact subcommand name has varied across `gcloud` releases; if the
above is rejected, consult `gcloud ai --help` and the logging docs for the
current invocation. Logging can also be enabled via the
`publisherModels.*` REST surface or, for deployed endpoints, in the Console.

### Using the Console

1. Open **Vertex AI** → **Model Garden** (publisher models) or **Online
   prediction** → your **Endpoint**.
2. Open the model/endpoint's **Logging** (or **Monitoring → Request-response
   logging**) settings.
3. Enable request-response logging, choose a **sampling rate**, and select the
   destination **BigQuery dataset** (Vertex creates the table on first write).
4. **Save.** The first rows can take a few minutes to appear after the next
   request.

## Finding the dataset and table

Vertex auto-names the destination per endpoint. With the canonical naming the
dataset is `logging_<endpoint-display-name>_<endpoint-id>` and the table is
`request_response_logging`; when you supply your own dataset (as in the gcloud
example above) the dataset is the one you named. List them with the `bq` CLI:

```bash
# Datasets in the project
bq ls --format=pretty my-gcp-project:

# Tables in the logging dataset
bq ls --format=pretty my-gcp-project:vertex_logging

# Inspect the schema before pointing Dressage at it
bq show --schema --format=prettyjson my-gcp-project:vertex_logging.request_response_logging
```

The canonical schema Dressage expects:

| Column | Type | Notes |
|--------|------|-------|
| `endpoint` | STRING | Full resource path; the model name is its trailing `models/<name>` segment |
| `deployed_model_id` | STRING | Deployment id (used as a model-name fallback) |
| `logging_time` | TIMESTAMP | Date-filter column |
| `request_id` | NUMERIC | Cast to STRING by Dressage; a synthetic id is used when absent |
| `request_payload` | STRING (REPEATED) | Serialized request JSON, usually one element |
| `response_payload` | STRING (REPEATED) | Serialized response JSON; one element per streamed chunk |

> **If your schema differs**, the column names are centralized in
> [`internal/vertexfetch/vertex.go`](../../internal/vertexfetch/vertex.go)
> (`buildQuery` and the `vertexRow` struct tags). The schema has changed over
> time, so adjust there if `bq show` reports different columns.

## Required IAM

The credential Dressage uses needs, at a minimum, to read the logging dataset
and to run query jobs:

- `roles/bigquery.dataViewer` on the logging **dataset** (read the rows).
- `roles/bigquery.jobUser` on the **project** (run the query job).

```bash
# Read access on the dataset (dataset-scoped via the BigQuery IAM API,
# or project-scoped as shown here for simplicity)
gcloud projects add-iam-policy-binding my-gcp-project \
  --member="serviceAccount:dressage@my-gcp-project.iam.gserviceaccount.com" \
  --role="roles/bigquery.dataViewer"

# Permission to run query jobs
gcloud projects add-iam-policy-binding my-gcp-project \
  --member="serviceAccount:dressage@my-gcp-project.iam.gserviceaccount.com" \
  --role="roles/bigquery.jobUser"
```

Scope `dataViewer` to the dataset rather than the project where you can, to
follow least privilege.

## Authentication

Authentication uses **Application Default Credentials (ADC)**, which resolve, in
order: the `GOOGLE_APPLICATION_CREDENTIALS` environment variable, `gcloud`
user credentials, and the attached workload identity when running inside GCP.

- **Running inside GCP** (GKE, Cloud Run, a GCE VM): prefer **workload
  identity** / the attached service account — no key files to manage or leak.
- **Running outside GCP**: use `gcloud auth application-default login` for
  interactive use, or pass `--credentials /path/to/key.json` to point at a
  service-account key file explicitly.

## Token accounting and the cache-write gap

Token counts come from the response `usageMetadata`:

| Gemini field | Dressage field |
|--------------|----------------|
| `promptTokenCount` | input tokens |
| `candidatesTokenCount` | output tokens |
| `cachedContentTokenCount` | cache-read tokens |

Gemini does **not** expose a cache-**write** counter (explicit caching is a
separate `cachedContents.create` call; `generateContent` reports only cache
reads). Dressage leaves cache-write at zero for Vertex, unlike the
Bedrock/Anthropic path which reports `cache_creation_input_tokens`.

For **streamed** responses logged as multiple chunks, Dressage aggregates the
chunks and reads `usageMetadata` from the final chunk. If a response carries no
`usageMetadata` at all (e.g. a streamed row logged without a final usage chunk),
its token totals are understated; Dressage prints **one** warning per run noting
how many rows were affected, rather than one line per row.

## Conversation grouping

As with the other providers, Dressage produces the rich turn-by-turn
**conversation reconstruction** (the drill-down `Detail`) only for
**session-grouped** conversations. Gemini has **no first-class equivalent** to
Anthropic's `metadata.user_id`, so there is no built-in per-request session id
to group on. Dressage handles this in two ways:

1. **Opt-in session marker (enables the drill-down).** If a wrapping harness
   embeds a `_session_<uuid>` marker in the request's `systemInstruction` text,
   Dressage extracts it, groups by session id (the same marker convention Claude
   Code uses elsewhere), and reconstructs the full turn-by-turn conversation for
   that session.
2. **Time-gap fallback (summary only).** Without a session marker, Dressage
   groups by `(provider, model, identity)` with a 5-minute gap between
   consecutive invocations — the same heuristic used for Bedrock and Azure.
   Because the request-response logging schema has no per-row caller identity
   (see below), the identity component is empty, so a day's Gemini invocations
   for one model are split into conversations purely by the time gap. These
   conversations appear in the report with their per-invocation request/response
   bodies, but **without** the reconstructed turn-by-turn `Detail` (consistent
   with the Bedrock and Azure behaviour). Embed the session marker if you want
   the drill-down.

## Identity attribution

The request-response logging schema carries **no per-row caller principal** (no
email or service-account identity). Dressage therefore leaves the identity
principal empty and instead records the **project**, **location**, and
**endpoint** (parsed from the `endpoint` resource path) as identity attributes.
Attributing invocations to a specific caller would require joining Cloud Audit
Logs, which is out of scope.

## Cost and data residency

- **BigQuery storage and query charges.** The logging table accrues storage
  charges per GB, and each `dressage vertex` run executes a query job billed by
  bytes scanned. Request/response payloads for long coding sessions can be large
  — narrow with `--start`/`--end`, and set a table expiration/partition policy
  appropriate to your retention needs. See
  [BigQuery pricing](https://cloud.google.com/bigquery/pricing).
- **Data residency.** Request/response payloads stored in BigQuery are subject
  to GCP's data-residency rules and live in the dataset's location. This is
  relevant for regulated workloads — choose the dataset location accordingly and
  treat the logged payloads as containing the full prompt and completion text.

## Usage

`--start`, `--end`, and `--output` are persistent (root) flags and may appear
before or after the `vertex` subcommand. The Vertex-specific flags are local to
`vertex`.

```bash
# Analyze all Gemini invocations in a logging table (ADC credentials)
dressage vertex \
  --project my-gcp-project \
  --dataset vertex_logging \
  --table request_response_logging

# Filter to a date range and pin the dataset location
dressage vertex \
  --project my-gcp-project \
  --dataset vertex_logging \
  --table request_response_logging \
  --location us-central1 \
  --start 2025-03-01 --end 2025-03-15

# Use an explicit service-account key and write to a named file
dressage vertex \
  --project my-gcp-project \
  --dataset vertex_logging \
  --table request_response_logging \
  --credentials /path/to/key.json \
  --output march-report.html
```

### Flags

Persistent (root) flags, shared with every provider:

| Flag | Default | Description |
|------|---------|-------------|
| `--start` | | Start date filter (YYYY-MM-DD, inclusive) |
| `--end` | | End date filter (YYYY-MM-DD, inclusive) |
| `--output` | `report.html` | Output HTML file path |

`vertex`-specific flags:

| Flag | Required | Default | Description |
|------|----------|---------|-------------|
| `--project` | Yes | | GCP project containing the BigQuery logging dataset |
| `--dataset` | Yes | | BigQuery dataset holding the request-response logs |
| `--table` | Yes | | BigQuery table holding the request-response logs |
| `--location` | No | | BigQuery dataset location (e.g. `us-central1`; inferred if empty) |
| `--credentials` | No | | Path to a service-account key JSON file (default: ADC) |

## Troubleshooting

- **No rows returned.** Request-response logging may not be enabled, the
  sampling rate may be 0, or you may be pointing at the wrong dataset/table.
  Confirm rows exist directly:
  `bq query --use_legacy_sql=false 'SELECT COUNT(*) FROM \`my-gcp-project.vertex_logging.request_response_logging\`'`.
- **Permission denied.** The credential lacks `roles/bigquery.dataViewer` on the
  dataset or `roles/bigquery.jobUser` on the project — see
  [Required IAM](#required-iam).
- **Schema mismatch / "Unrecognized name" query error.** The logging schema has
  changed; compare `bq show --schema` against the
  [expected columns](#finding-the-dataset-and-table) and adjust `buildQuery` /
  the `vertexRow` tags in `internal/vertexfetch/vertex.go`.
- **Claude / Anthropic rows are not reconstructed.** Expected — Anthropic-on-
  Vertex reconstruction is deferred to
  [#4](https://github.com/rxbynerd/dressage/issues/4). Those rows still appear in
  the summary statistics; Dressage logs a one-line notice when it skips them.
- **Token totals look low for some invocations.** Those responses carried no
  `usageMetadata` (commonly streamed rows without a final usage chunk); Dressage
  warns once per run with the affected count.
