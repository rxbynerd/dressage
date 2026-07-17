# AWS Bedrock

Analyze [AWS Bedrock model invocation logs](https://docs.aws.amazon.com/bedrock/latest/userguide/model-invocation-logging.html)
that Bedrock delivers as gzipped NDJSON files to an S3 bucket.

## Prerequisites

- Go 1.23+ (only to build from source).
- Bedrock model invocation logging enabled and configured to deliver to S3
  (see below).
- AWS credentials resolvable by the AWS SDK (environment variables, a shared
  config/credentials profile, SSO, or an instance/role) with the following
  permissions on the log bucket:
  - `s3:ListBucket` on the bucket, to enumerate log objects.
  - `s3:GetObject` on the log objects, to download log files **and** any
    overflow payload objects (see [Overflow payloads](#overflow-payloads)).

## Enabling Bedrock invocation logging

1. Open the **Amazon Bedrock** console and go to **Settings** → **Model
   invocation logging**.
2. Toggle **Model invocation logging** on.
3. Under **Select the data types to include**, ensure **Text** is enabled so
   request and response bodies are captured (this is what Dressage reconstructs
   conversations from).
4. Choose **S3 only** (or S3 + CloudWatch) as the destination and select the
   destination bucket and an optional key prefix.
5. Bedrock will validate that it can write to the bucket. If prompted, accept
   the suggested bucket policy that grants the Bedrock service
   `s3:PutObject` on the destination.
6. Save. New invocations begin landing in S3 within a few minutes.

For the authoritative steps and the exact IAM/bucket policies AWS requires for
the *writer* (the Bedrock service), see the
[AWS documentation](https://docs.aws.amazon.com/bedrock/latest/userguide/model-invocation-logging.html).
Dressage itself only needs *read* access (listed above).

## S3 key layout

Bedrock writes one gzipped NDJSON object per delivery window under a path keyed
by account, region, and the UTC hour:

```
s3://{bucket}/{prefix}/AWSLogs/{accountId}/BedrockModelInvocationLogs/{region}/YYYY/MM/DD/HH/*.json.gz
```

Dressage lists every `*.json.gz` object under the configured prefix, then uses
the `YYYY/MM/DD` path components to filter by `--start`/`--end` before
downloading. Objects whose date cannot be parsed from the key are kept rather
than silently dropped.

Each record in the NDJSON stream contains the request and response JSON bodies,
input/output token counts (including cache-read and cache-write counts), the
model ID, the operation type (`Converse`, `ConverseStream`, `InvokeModel`,
`InvokeModelWithResponseStream`), the caller identity ARN, the account ID,
region, and a timestamp.

## Overflow payloads

When a request or response body exceeds Bedrock's inline limit (~100KB), Bedrock
does not embed the body in the log record. Instead it writes the body to a
separate S3 object (typically under a `data/` subdirectory) and records a
reference in `inputBodyS3Path` / `outputBodyS3Path`. Dressage detects these
references and fetches the referenced objects to inline the full body before
reconstruction — which is why the read permissions above must cover the whole
bucket, not just the top-level log objects.

## Usage

`--start`, `--end`, `--out`, and `--raw-bodies` are ingestion flags shared by
every provider subcommand. The S3-specific flags are local to `bedrock`.

```bash
# Analyze all logs in a bucket
dressage bedrock --bucket my-bedrock-logs

# Filter to a date range using a named AWS profile
dressage bedrock --bucket my-bedrock-logs --profile dev --start 2025-03-01 --end 2025-03-15

# Specify a prefix, region, and IR output directory
dressage bedrock --bucket my-bedrock-logs --prefix prod/AWSLogs --region us-east-1 --out march.ir
```

### Flags

Ingestion flags, shared by every provider subcommand:

| Flag | Default | Description |
|------|---------|-------------|
| `--start` | | Start date filter (YYYY-MM-DD, inclusive) |
| `--end` | | End date filter (YYYY-MM-DD, inclusive) |
| `--out` | `report.ir` | IR output directory |
| `--raw-bodies` | `omit` | Embed verbatim request/response JSON in the IR: `omit` or `embed` |

`bedrock`-specific flags:

| Flag | Required | Default | Description |
|------|----------|---------|-------------|
| `--bucket` | Yes | | S3 bucket containing Bedrock invocation logs |
| `--prefix` | No | `""` | S3 key prefix (e.g. `prod/AWSLogs`) |
| `--region` | No | from env/config | AWS region |
| `--profile` | No | | AWS named profile |
