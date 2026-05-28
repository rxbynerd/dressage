# dressage

Analyze hosted LLM model invocation logs to investigate opportunities for developing and improving coding harnesses. Dressage fetches logs from a provider, normalizes them into a provider-neutral record, groups them into conversations, and generates a self-contained HTML report with per-day summaries and drill-down into individual request/response pairs. The conversation grouping and HTML drill-down are provider-agnostic; only the fetcher and the on-the-wire log schema differ per provider. AWS Bedrock is the first supported provider, with others on the way.

## Supported providers

| Provider | Status | Subcommand | Docs |
|----------|--------|------------|------|
| AWS Bedrock | Supported | `dressage bedrock` | [docs/providers/bedrock.md](docs/providers/bedrock.md) |
| Azure OpenAI | Planned ([#5](https://github.com/rxbynerd/dressage/issues/5)) | — | [docs/providers/azure.md](docs/providers/azure.md) |
| Vertex AI / Gemini | Planned ([#6](https://github.com/rxbynerd/dressage/issues/6)) | — | [docs/providers/vertex.md](docs/providers/vertex.md) |

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
```

### Flags

`--start`, `--end`, and `--output` are persistent (root) flags shared by every
provider and may be given before or after the subcommand:

| Flag | Default | Description |
|------|---------|-------------|
| `--start` | | Start date filter (YYYY-MM-DD, inclusive) |
| `--end` | | End date filter (YYYY-MM-DD, inclusive) |
| `--output` | `report.html` | Output HTML file path |

The `bedrock` subcommand adds S3-specific flags:

| Flag | Required | Default | Description |
|------|----------|---------|-------------|
| `--bucket` | Yes | | S3 bucket containing Bedrock invocation logs |
| `--prefix` | No | `""` | S3 key prefix (e.g. `prod/AWSLogs`) |
| `--region` | No | from env | AWS region |
| `--profile` | No | | AWS named profile |

### Examples

```bash
# Analyze all logs in a bucket
dressage bedrock --bucket my-bedrock-logs

# Filter to a specific date range using a named AWS profile
dressage bedrock --bucket my-bedrock-logs --profile dev --start 2025-03-01 --end 2025-03-15

# Specify a prefix and output path
dressage bedrock --bucket my-bedrock-logs --prefix prod/AWSLogs --output march-report.html
```

See [docs/providers/bedrock.md](docs/providers/bedrock.md) for how Bedrock
invocation logging works, how to enable it, the S3 key layout, and overflow
payload handling.

## Report Structure

The generated HTML report is a single self-contained file (no external dependencies) with three levels of drill-down:

1. **Header** - Overall stats: total invocations, tokens, errors, date range, breakdowns by model and operation type
2. **Day cards** - One collapsible section per day showing invocation count, token totals, and conversation count
3. **Conversations** - Within each day, records are grouped into conversations (same provider, model, and identity, within 5-minute gaps). Shows message count, token usage, and time range
4. **Invocations** - Each individual request/response pair with pretty-printed JSON bodies, operation type, status, and token counts

All drill-down uses native HTML `<details>/<summary>` elements - no JavaScript required.

## License

MIT
