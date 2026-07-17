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
provider Fetcher ──► []model.Record ──► summary.NewPlan ──► Plan.Conversations ──┬─► ir.Exporter   (streamed, one conv at a time)
  (per-provider)      (normalized;        (metadata-only      (materialize:        └─► report.Generate (HTML, via retained *model.Report)
                       bodies may be       grouping, zero      reconstruct,
                       lazy)               body loads)         render, yield)
```

Everything after a Fetcher is **provider-neutral**. Only the fetcher and the
on-the-wire log schema differ per provider. Record bodies are lazy
(`model.Body.Source`/`Load()`): the claude fetcher hands out file-backed
references so nothing holds transcript bytes until a conversation
materializes; the cloud fetchers keep inline bodies behind the same interface.
`summary.Summarize` survives as the retain-everything wrapper the HTML path
uses.

## Project structure

```
cmd/dressage/main.go      - cobra CLI: provider subcommands, shared runReport, --format/--ir-dir flags
internal/
  model/                  - provider-neutral data types (only logic: Body.Load/Present lazy indirection)
    record.go             - Record, Identity, Body (+BodySource), Correlation (one normalized invocation)
    conversation.go       - ConversationDetail, Thread, Turn, ContentBlock, TurnMetrics, ToolDef (reconstructed view)
    summary.go            - Report, DaySummary, ConversationSummary (+Sidechains), Invocation, Stats (render/export model)
  fetch/fetch.go          - Fetcher interface: Fetch(ctx, start, end) ([]model.Record, error)  ← the provider abstraction
  s3fetch/                - AWS Bedrock: S3 .json.gz NDJSON (bedrock.go = schema, fetch.go = listing/parse)
  azurefetch/             - Azure OpenAI: Log Analytics workspace AND Storage-account blobs
  vertexfetch/            - Google Vertex AI / Gemini: BigQuery request-response table
  rawfetch/               - local raw Claude API bodies: mtime windowing, chain-aware previous_message_id
                            pairing (threads/sidechains), lazy file-backed bodies (lazy.go)
  conversation/           - reconstruct ConversationDetail from records; dispatch by envelope family
    dispatch.go           - family(provider, modelID) → Anthropic | OpenAI | Gemini | VertexDeferred; ExtractSessionID
    threads.go            - ReconstructThreads: main thread + sidechains via Correlation.ThreadID
    parse.go              - Anthropic Messages API envelope (count-first: parses only the fullest request)
    openai.go/openai_stream.go/stream.go - OpenAI Chat Completions envelope
    gemini.go             - Vertex generateContent (contents[]/parts[]) envelope
  summary/summary.go      - NewPlan (metadata-only grouping) + Plan.Conversations (per-conv
                            materialization, iter.Seq); Summarize = retaining wrapper
  report/                 - report.go + template.html (self-contained HTML via embed.FS)
  ir/                     - IR exporter: ir.go (schema types), map.go (pure model→ir mappers),
                            facts.go/turns.go (Parquet row mappers), export.go (streaming Exporter + manifest)
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
- **Two outputs, one pipeline.** `report.Generate` (HTML, for humans) and the
  streaming `ir.Exporter` (IR directory, for downstream programs) both consume
  the same materialization pass. The IR is the durable external contract:
  versioned (`dressage.ir/1.1`), snake_case JSON per conversation +
  `manifest.json` + two Parquet tables (`facts.parquet` per invocation,
  `turns.parquet` per deduplicated turn incl. sidechains). Raw wire bodies are
  opt-in (`--raw-bodies embed`); the manifest records the mode.

## Conventions for agentic sessions

- Module path is `github.com/rxbynerd/dressage` (rxbynerd, intentional — see
  global CLAUDE.md on the username distinction).
- **Tests** are table-driven with **golden files**:
  `internal/report/testdata/golden_report.html`,
  `internal/ir/testdata/golden_ir/`, and decoded-row JSON goldens for the
  Parquet tables (`golden_facts.json`, `golden_turns.json` — Parquet bytes are
  not stable across library versions, so never golden the bytes). Regenerate
  goldens ONLY for an intentional output change, and say why in the commit.
  Fetchers use small fake API interfaces (e.g. `s3API`) for testability —
  follow that pattern.
- **`internal/ir`** keeps `model→ir` translation in pure mappers (`map.go`,
  `facts.go`, `turns.go` — no direct IO; body payloads load only through
  `model.Body.Load`); `export.go`'s `Exporter` is the only IO (mirrors the
  `report.Generate`/`Render` split). On-disk filenames are a sanitized
  transform of the conversation id; consumers must resolve files via the
  manifest `file`/`files` fields, never by rebuilding from id.
- Keep `docs/providers/*.md` and `CHANGELOG.md` (Keep a Changelog format)
  current when you change a fetcher or user-facing behaviour.

## Architecture decisions (durable rationale)

- **Single self-contained HTML file** — inlined CSS, zero JS, `<details>/<summary>`
  drill-down — so a report is trivial to share, attach to a ticket, or archive.
- **Conversation grouping** — session id when present (Claude Code embeds it in
  Anthropic `metadata.user_id`, OpenAI `user`, Gemini `systemInstruction`),
  otherwise `(provider, modelId, principal)` + a 5-minute gap. Gap constant lives
  in `internal/summary/summary.go`.
- **`json.RawMessage` for bodies, behind a lazy handle** — request/response
  bodies vary by model and operation, so they are carried raw: pretty-printed
  for HTML, embedded inline for the IR when requested. `Body.Source`
  (`BodySource.Load()`) lets a fetcher defer the payload to disk; all readers
  go through `Body.Load()`/`Present()`.
- **Bounded memory, not streaming fetch** — `Fetch` still returns the whole
  `[]model.Record`, but records are small metadata (~250 B) once bodies are
  lazy; grouping (`summary.NewPlan`) does zero body loads and conversations
  materialize one at a time into the streaming IR exporter. Peak memory ≈ the
  largest single transcript (measured: the whole 25 GB claude capture exports
  with ~700 MB peak). The HTML path still retains every rendered conversation —
  a fully-streaming HTML writer is an explicit non-goal for now.
- **IR is versioned and provider-neutral** — a downstream analysis program can
  consume conversations without re-parsing provider-native logs.
- **Rendered body cap** — `summary.renderBody` truncates each raw-invocation
  body embedded in the HTML report to `maxRenderedBodyBytes` (32 KiB) with a
  marker. This is a defensive cap for providers (like `claude`) that resend the
  full running transcript on every turn, which would otherwise produce
  multi-GB reports; reconstruction and the IR are unaffected since they read
  the raw JSON, not the rendered string.
