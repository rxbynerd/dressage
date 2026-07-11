# dressage

Analyze hosted LLM model invocation logs to investigate opportunities for developing and improving coding harnesses. Dressage fetches logs from a provider, normalizes them into a provider-neutral record, groups them into conversations, and generates a self-contained HTML report with per-day summaries and drill-down into individual request/response pairs. The conversation grouping and HTML drill-down are provider-agnostic; only the fetcher and the on-the-wire log schema differ per provider. AWS Bedrock is the first supported provider, with others on the way.

## Supported providers

| Provider | Status | Subcommand | Docs |
|----------|--------|------------|------|
| AWS Bedrock | Supported | `dressage bedrock` | [docs/providers/bedrock.md](docs/providers/bedrock.md) |
| Azure OpenAI (Log Analytics) | Supported | `dressage azure` | [docs/providers/azure.md](docs/providers/azure.md) |
| Azure OpenAI (Storage account) | Supported | `dressage azure-storage` | [docs/providers/azure.md](docs/providers/azure.md#storage-account-destination) |
| Vertex AI / Gemini (BigQuery) | Supported | `dressage vertex` | [docs/providers/vertex.md](docs/providers/vertex.md) |
| Claude API (raw bodies, local) | Supported | `dressage claude` | [docs/providers/claude.md](docs/providers/claude.md) |

## Prerequisites

- Go 1.23+ (only to build from source)
- Credentials and read access for the provider you are analyzing (see the
  provider's docs page, e.g. [Bedrock](docs/providers/bedrock.md))

## Installation

```bash
go install github.com/rxbynerd/dressage/cmd/dressage@latest
```

Or build from source:

```bash
git clone https://github.com/rxbynerd/dressage.git
cd dressage
go build -o dressage ./cmd/dressage/
```

## Usage

Dressage uses a provider subcommand to choose the log source. Running `dressage`
with no subcommand prints help.

```bash
dressage bedrock --bucket my-bedrock-logs [flags]
dressage azure --workspace <log-analytics-workspace-id> [flags]
dressage azure-storage --account <storage-account-name> [flags]
dressage vertex --project <gcp-project> --dataset <ds> --table <table> [flags]
dressage claude [--dir ~/.claude/raw-api-bodies] [flags]
```

### Flags

`--start`, `--end`, `--output`, `--format`, and `--ir-dir` are persistent (root)
flags shared by every provider and may be given before or after the subcommand:

| Flag | Default | Description |
|------|---------|-------------|
| `--start` | | Start date filter (YYYY-MM-DD, inclusive) |
| `--end` | | End date filter (YYYY-MM-DD, inclusive) |
| `--output` | `report.html` | Output HTML file path |
| `--format` | `html` | Output format: `html`, `ir`, or `both` (see [Outputs](#outputs)) |
| `--ir-dir` | derived | IR output directory (default: `--output` with its extension replaced by `.ir`) |

The `bedrock` subcommand adds S3-specific flags:

| Flag | Required | Default | Description |
|------|----------|---------|-------------|
| `--bucket` | Yes | | S3 bucket containing Bedrock invocation logs |
| `--prefix` | No | `""` | S3 key prefix (e.g. `prod/AWSLogs`) |
| `--region` | No | from env | AWS region |
| `--profile` | No | | AWS named profile |

The `azure` subcommand adds Log Analytics-specific flags:

| Flag | Required | Default | Description |
|------|----------|---------|-------------|
| `--workspace` | Yes | | Log Analytics workspace ID (GUID) |
| `--subscription` | No | | Subscription ID narrowing filter |
| `--resource` | No | | Azure OpenAI resource ID (or substring) narrowing filter |
| `--tenant` | No | | Microsoft Entra tenant ID for authentication |

The `azure-storage` subcommand reads the same logs exported to a storage account
and adds these flags:

| Flag | Required | Default | Description |
|------|----------|---------|-------------|
| `--account` | Yes | | Storage account name holding the diagnostic logs |
| `--container` | No | `insights-logs-requestresponse` | Blob container holding the logs |
| `--tenant` | No | | Microsoft Entra tenant ID for authentication |

The `vertex` subcommand reads Vertex AI request-response logs from BigQuery and
adds these flags:

| Flag | Required | Default | Description |
|------|----------|---------|-------------|
| `--project` | Yes | | GCP project containing the BigQuery logging dataset |
| `--dataset` | Yes | | BigQuery dataset holding the request-response logs |
| `--table` | Yes | | BigQuery table holding the request-response logs |
| `--location` | No | | BigQuery dataset location (e.g. `us-central1`; inferred if empty) |
| `--credentials` | No | | Path to a service-account key JSON file (default: ADC) |

Gemini invocations are reconstructed into full conversations; Claude-on-Vertex
invocations contribute to summary stats but are not yet reconstructed (tracked
in [#4](https://github.com/rxbynerd/dressage/issues/4)).

The `claude` subcommand reconstructs conversations from raw Anthropic Messages
API request/response bodies captured on the local filesystem (as written by
Claude Code under `~/.claude/raw-api-bodies`). It needs no cloud credentials and
adds a single flag:

| Flag | Required | Default | Description |
|------|----------|---------|-------------|
| `--dir` | No | `~/.claude/raw-api-bodies` | Directory of captured request/response bodies |

This capture can be very large; always scope with `--start`/`--end` (a single day
is a good working unit). See [docs/providers/claude.md](docs/providers/claude.md).

### Examples

```bash
# Analyze all logs in a bucket
dressage bedrock --bucket my-bedrock-logs

# Filter to a specific date range using a named AWS profile
dressage bedrock --bucket my-bedrock-logs --profile dev --start 2025-03-01 --end 2025-03-15

# Specify a prefix and output path
dressage bedrock --bucket my-bedrock-logs --prefix prod/AWSLogs --output march-report.html

# Analyze Azure OpenAI logs from a Log Analytics workspace
dressage azure --workspace 11111111-2222-3333-4444-555555555555

# Filter to a date range and narrow to one resource
dressage azure --workspace 11111111-2222-3333-4444-555555555555 \
  --resource my-aoai --start 2025-03-01 --end 2025-03-15

# Analyze Azure OpenAI logs exported to a storage account
dressage azure-storage --account mydiaglogs --start 2025-03-01 --end 2025-03-15

# Analyze Vertex AI / Gemini logs from a BigQuery dataset
dressage vertex --project my-gcp-project --dataset vertex_logging \
  --table request_response_logging --start 2025-03-01 --end 2025-03-15

# Analyze one day of raw Claude API bodies captured locally by Claude Code
dressage claude --start 2025-03-01 --end 2025-03-01
```

See [docs/providers/bedrock.md](docs/providers/bedrock.md) for how Bedrock
invocation logging works, [docs/providers/azure.md](docs/providers/azure.md)
for how to enable Azure OpenAI diagnostic logging, the content-logging caveat,
and required RBAC, and [docs/providers/vertex.md](docs/providers/vertex.md) for
enabling Vertex AI request-response logging, the required IAM, and the Gemini
session-grouping and cache-write caveats.

## Outputs

Dressage produces two kinds of output from a single fetch, selected with
`--format`:

| `--format` | Writes | For |
|------------|--------|-----|
| `html` (default) | the HTML report at `--output` | humans |
| `ir` | the IR directory only | downstream tooling |
| `both` | the HTML report **and** the IR directory | both, from one fetch |

Ingestion (S3 / BigQuery / Log Analytics calls) is the expensive part, so
`--format both` is the way to get both artifacts without fetching twice.

The **IR** (Intermediate Representation) is a stable, versioned, provider-neutral
JSON export of the same conversations the report shows, intended for a separate
analysis program (judging, classification, signal extraction) to consume without
re-fetching or re-parsing provider-native logs. Its directory layout is:

```
report.ir/
├── manifest.json                    # run metadata + index of all conversations
└── conversations/
    ├── <id>.json                    # one self-contained conversation IR per file
    └── …
```

`--ir-dir` overrides the destination; by default it is the `--output` path with
its extension replaced by `.ir` (so `--output march.html` yields `march.ir/`).
Each conversation file carries both the reconstructed conversation
(system prompt, tools, turns of typed blocks, per-turn metrics) and the raw
per-invocation request/response pairs with the provider JSON bodies embedded
inline. The full schema — every field, the block-type table, the stable-id rule,
and the versioning policy — is documented in [docs/ir-format.md](docs/ir-format.md).

```bash
# Emit both the human report and the machine IR from one fetch.
dressage bedrock --bucket my-bedrock-logs \
  --start 2026-05-01 --end 2026-05-31 \
  --format both --output may-report.html      # IR -> ./may-report.ir/
```

> The IR contains full prompts and tool I/O verbatim (the same content the HTML
> report exposes). v1 performs no redaction; treat IR directories as sensitive.

## Report Structure

The generated HTML report is a single self-contained file (no external dependencies) with three levels of drill-down:

1. **Header** - Overall stats: total invocations, tokens, errors, date range, breakdowns by model and operation type
2. **Day cards** - One collapsible section per day showing invocation count, token totals, and conversation count
3. **Conversations** - Within each day, records are grouped into conversations (same provider, model, and identity, within 5-minute gaps). Shows message count, token usage, and time range
4. **Invocations** - Each individual request/response pair with pretty-printed JSON bodies, operation type, status, and token counts

All drill-down uses native HTML `<details>/<summary>` elements - no JavaScript required.

## License

MIT
