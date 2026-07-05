# dressage - Development Guide

## Build & Run

```bash
go build -o dressage ./cmd/dressage/    # build
go vet ./...                             # lint
go test ./...                            # test (when tests exist)
```

## Project Structure

```
cmd/dressage/main.go       - CLI entrypoint (cobra), flag parsing, pipeline orchestration
internal/
  model/
    log.go                 - Go types mapping to the Bedrock invocation log JSON schema
    summary.go             - Report/summary data types for template rendering
  rawfetch/
    rawfetch.go            - Local raw Claude API body ingestion: dir walk, mtime windowing, concurrent parse
    parse.go               - Request/response decoding, previous_message_id pairing, record building
  s3fetch/
    fetch.go               - S3 listing, download, gzip decompression, NDJSON parsing
  summary/
    summary.go             - Conversation grouping (model+ARN, 5min gap) and stats aggregation
  report/
    report.go              - HTML generation using embed.FS + html/template
    template.html          - Self-contained HTML template (all CSS inlined)
```

## Key Packages

- **internal/model** - Shared types. `InvocationLog` mirrors the AWS schema; `Report`/`DaySummary`/`ConversationSummary`/`Invocation` are the rendering model.
- **internal/s3fetch** - Uses AWS SDK v2. `Fetcher.Fetch()` lists, downloads, and parses `.json.gz` files. Filters by date using S3 key path structure. Uses an `s3API` interface for testability.
- **internal/summary** - `Summarize()` takes raw logs and produces a `Report`. Conversation grouping heuristic: same `(modelId, identity.arn)` pair with <5min gap between invocations.
- **internal/report** - `Generate()` writes HTML to a file. Template is embedded via `//go:embed`. Uses `<details>/<summary>` for zero-JS drill-down.

## Architecture Decisions

- **Single HTML file**: The report is fully self-contained with inlined CSS, no external JS/CSS dependencies. This makes it easy to share, attach to tickets, or archive.
- **Conversation grouping**: Uses a simple heuristic (same model + ARN + 5min gap). This works well for coding harness analysis where sessions have clear boundaries. The gap threshold is a constant in `internal/summary/summary.go`.
- **NDJSON parsing**: Bedrock S3 logs are newline-delimited JSON in gzipped files. Scanner buffer is set to 10MB to handle large log lines.
- **No streaming**: Logs are loaded entirely into memory. This is fine for typical analysis volumes (days/weeks of logs) but would need rethinking for months of high-volume data.
- **json.RawMessage for bodies**: Input/output JSON bodies vary by model and API operation, so they're stored as raw JSON and pretty-printed at render time.
- **Raw Claude bodies (rawfetch)**: The `claude` subcommand reads local `~/.claude/raw-api-bodies` files. Request/response filenames share no key, so a request is paired to its response via the `diagnostics.previous_message_id` chain (turn i's response = turn i+1's `previous_message_id`), with an mtime fallback for each session's terminal turn. Timestamps come from file mtime (bodies carry none). Session id is read from the JSON-object `metadata.user_id`. These captures are huge and Claude Code resends the full transcript each turn, so scope with `--start`/`--end`.
- **Rendered body cap**: `summary.renderBody` truncates each raw-invocation body embedded in the report to `maxRenderedBodyBytes` (32 KiB) with a marker. This is a defensive cap for the resend-full-transcript pattern (which would otherwise produce multi-GB reports); reconstruction is unaffected because it reads the raw JSON, not the rendered string.
