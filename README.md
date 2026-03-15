# dressage

Analyze AWS Bedrock model invocation logs to investigate opportunities for developing and improving coding harnesses. Fetches logs from S3, groups them into conversations, and generates a self-contained HTML report with per-day summaries and drill-down into individual request/response pairs.

## Prerequisites

- Go 1.23+
- AWS credentials with S3 read access to your Bedrock log bucket
- [Bedrock model invocation logging](https://docs.aws.amazon.com/bedrock/latest/userguide/model-invocation-logging.html) enabled and configured to write to S3

## Installation

```bash
go install github.com/rubynerd/dressage/cmd/dressage@latest
```

Or build from source:

```bash
git clone https://github.com/rubynerd/dressage.git
cd dressage
go build -o dressage ./cmd/dressage/
```

## Usage

```bash
dressage --bucket my-bedrock-logs [flags]
```

### Flags

| Flag | Required | Default | Description |
|------|----------|---------|-------------|
| `--bucket` | Yes | | S3 bucket containing Bedrock invocation logs |
| `--prefix` | No | `""` | S3 key prefix (e.g., `logs/production`) |
| `--region` | No | from env | AWS region |
| `--profile` | No | | AWS named profile |
| `--start` | No | | Start date filter (YYYY-MM-DD, inclusive) |
| `--end` | No | | End date filter (YYYY-MM-DD, inclusive) |
| `--output` | No | `report.html` | Output HTML file path |

### Examples

```bash
# Analyze all logs in a bucket
dressage --bucket my-bedrock-logs

# Filter to a specific date range using a named AWS profile
dressage --bucket my-bedrock-logs --profile dev --start 2025-03-01 --end 2025-03-15

# Specify a prefix and output path
dressage --bucket my-bedrock-logs --prefix prod/AWSLogs --output march-report.html
```

## Report Structure

The generated HTML report is a single self-contained file (no external dependencies) with three levels of drill-down:

1. **Header** - Overall stats: total invocations, tokens, errors, date range, breakdowns by model and operation type
2. **Day cards** - One collapsible section per day showing invocation count, token totals, and conversation count
3. **Conversations** - Within each day, logs are grouped into conversations (same model + identity ARN, within 5-minute gaps). Shows message count, token usage, and time range
4. **Invocations** - Each individual request/response pair with pretty-printed JSON bodies, operation type, status, and token counts

All drill-down uses native HTML `<details>/<summary>` elements - no JavaScript required.

## How Bedrock Logging Works

Bedrock stores invocation logs as gzipped NDJSON files in S3 at:

```
s3://{bucket}/{prefix}/AWSLogs/{accountId}/BedrockModelInvocationLogs/{region}/YYYY/MM/DD/HH/*.json.gz
```

Each log record contains the full request and response JSON bodies (up to 100KB), token counts, model ID, operation type (Converse, ConverseStream, InvokeModel, InvokeModelWithResponseStream), caller identity, and timestamps.

To enable logging, see the [AWS documentation](https://docs.aws.amazon.com/bedrock/latest/userguide/model-invocation-logging.html).

## License

MIT
