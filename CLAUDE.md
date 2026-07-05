# dressage — Development Guide

Dressage fetches hosted-LLM invocation logs from a provider gateway, normalizes
them into a provider-neutral record, groups them into conversations, and emits
analysis-ready outputs. Its purpose is to help develop and improve coding
harnesses by examining what happened over the wire.

**Scope:** dressage is deliberately an **ingest → normalize → export** tool.
Conversation *analysis* (judging/classifying/scoring) is intentionally left to
separate downstream programs that consume the exported IR — do not add analysis
features into dressage itself. See `docs/ir-exporter-proposal.md`.

## Build, test, lint (run this gate before every commit)

```bash
go build ./...      # build (Go 1.25; binary: go build -o dressage ./cmd/dressage/)
go vet ./...        # vet
gofmt -l .          # format check (should print nothing)
go test ./...       # tests
```

Don't commit failing code. Make logical, well-explained commits as you work.

## Pipeline (mental model)

```
provider Fetcher ──► []model.Record ──► summary.Summarize ──► *model.Report ──┬─► report.Generate  (HTML)
  (per-provider)      (normalized)        (days+conversations,                └─► ir.Export        (IR dir)
                                           reconstruction attached)
```

Everything after a Fetcher is **provider-neutral**. Only the fetcher and the
on-the-wire log schema differ per provider.

## Project structure

```
cmd/dressage/main.go      - cobra CLI: provider subcommands, shared runReport, --format/--ir-dir flags
internal/
  model/                  - provider-neutral data types (no logic)
    record.go             - Record, Identity, Body (one normalized invocation)
    conversation.go       - ConversationDetail, Turn, ContentBlock, TurnMetrics, ToolDef (reconstructed view)
    summary.go            - Report, DaySummary, ConversationSummary, Invocation, Stats (render/export model)
  fetch/fetch.go          - Fetcher interface: Fetch(ctx, start, end) ([]model.Record, error)  ← the provider abstraction
  s3fetch/                - AWS Bedrock: S3 .json.gz NDJSON (bedrock.go = schema, fetch.go = listing/parse)
  azurefetch/             - Azure OpenAI: Log Analytics workspace AND Storage-account blobs
  vertexfetch/            - Google Vertex AI / Gemini: BigQuery request-response table
  conversation/           - reconstruct ConversationDetail from records; dispatch by envelope family
    dispatch.go           - family(provider, modelID) → Anthropic | OpenAI | Gemini | VertexDeferred; ExtractSessionID
    parse.go              - Anthropic Messages API envelope
    openai.go/openai_stream.go/stream.go - OpenAI Chat Completions envelope
    gemini.go             - Vertex generateContent (contents[]/parts[]) envelope
  summary/summary.go      - group records into days + conversations; attach reconstruction
  report/                 - report.go + template.html (self-contained HTML via embed.FS)
  ir/                     - IR exporter: ir.go (schema types), map.go (pure model→ir mappers), export.go (Export + manifest)
docs/
  providers/<p>.md        - per-provider setup/auth/log-schema notes (keep current when changing a fetcher)
  ir-exporter-proposal.md - IR design rationale;  ir-format.md - IR consumer contract
```

## Key abstractions

- **Adding a provider** = a new `internal/<p>fetch` package implementing
  `fetch.Fetcher` (normalize the gateway's format to `[]model.Record`) plus a
  subcommand in `main.go`. Nothing downstream changes.
- **Conversation reconstruction** dispatches on `(provider, modelID)` to an
  envelope family. Non-Gemini Vertex (Claude-on-Vertex) reconstruction is
  deferred (issue #4): those records still appear in summary stats, but their
  `ConversationDetail` is nil (and the IR emits `conversation: null`).
- **Two outputs, one pipeline.** `report.Generate` (HTML, for humans) and
  `ir.Export` (IR directory, for downstream programs) both consume the same
  `*model.Report`. The IR is the durable external contract: versioned
  (`dressage.ir/1.0`), snake_case JSON, a per-conversation file + `manifest.json`.

## Conventions for agentic sessions

- Module path is `github.com/rxbynerd/dressage` (rxbynerd, intentional — see
  global CLAUDE.md on the username distinction).
- **Tests** are table-driven with **golden files**:
  `internal/report/testdata/golden_report.html` and
  `internal/ir/testdata/golden_ir/`. Regenerate goldens ONLY for an intentional
  output change, and say why in the commit. Fetchers use small fake API
  interfaces (e.g. `s3API`) for testability — follow that pattern.
- **`internal/ir`** keeps all `model→ir` translation pure in `map.go`;
  `export.go` is the only IO (mirrors the `report.Generate`/`Render` split).
  On-disk filenames are a sanitized transform of the conversation id; consumers
  must resolve files via the manifest `file` field, never by rebuilding from id.
- Keep `docs/providers/*.md` and `CHANGELOG.md` (Keep a Changelog format)
  current when you change a fetcher or user-facing behaviour.

## Architecture decisions (durable rationale)

- **Single self-contained HTML file** — inlined CSS, zero JS, `<details>/<summary>`
  drill-down — so a report is trivial to share, attach to a ticket, or archive.
- **Conversation grouping** — session id when present (Claude Code embeds it in
  Anthropic `metadata.user_id`, OpenAI `user`, Gemini `systemInstruction`),
  otherwise `(provider, modelId, principal)` + a 5-minute gap. Gap constant lives
  in `internal/summary/summary.go`.
- **`json.RawMessage` for bodies** — request/response bodies vary by model and
  operation, so they are carried raw: pretty-printed for HTML, embedded inline
  for the IR.
- **No streaming** — logs are loaded fully into memory. Fine for typical
  days/weeks volumes; would need rethinking for months of high-volume data.
- **IR is versioned and provider-neutral** — a downstream analysis program can
  consume conversations without re-parsing provider-native logs.
