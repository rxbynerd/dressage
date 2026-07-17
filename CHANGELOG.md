# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- IR schema `dressage.ir/1.2` (additive): reconstructed **sidechains** (subagent
  threads) are now carried inline in each conversation file
  (`conversation.sidechains[]`), so a consumer renders or analyses subagents
  without joining `turns.parquet`; the manifest's `totals` gained
  `model_breakdown` / `op_breakdown` maps (per-model and per-operation invocation
  counts across the run); and each `conversations[]` index entry gained
  `display_id`. See [docs/ir-format.md](docs/ir-format.md).
- Columnar IR tables for analytical engines (IR schema `dressage.ir/1.1`):
  every IR export now includes `facts.parquet` (one row per invocation —
  errored and sidechain invocations included — with conversation/session/
  thread membership, token and cache counters, status, stop reason, and the
  claude provider's message-id correlation columns) and `turns.parquet` (one
  row per deduplicated reconstructed turn, main thread **and** subagent
  sidechains, with a flattened full-text-searchable `text` column and the
  typed blocks as JSON). Both are zstd-compressed, content-deterministic, and
  resolved via the manifest's new `files` field. A DuckDB cookbook —
  spend/cache/error/session-shape queries, tool-use frequency, FTS setup, and
  web-UI wiring guidance — ships in
  [docs/duckdb-cookbook.md](docs/duckdb-cookbook.md). Adds the pure-Go
  `github.com/parquet-go/parquet-go` dependency.
- Sidechain (subagent) reconstruction for the `claude` provider: a session's
  requests are split into linear chains by first-message identity, pairing
  runs per chain (fixing response/token misattribution when parallel subagent
  chains interleaved with the main thread), compaction-split chains are
  stitched back into one thread via `previous_message_id`, and every chain
  beyond the main thread is reconstructed as a sidechain and exported in
  `turns.parquet` with its thread id. On a real multi-agent capture sidechains
  were ~57% of all deduplicated turns — content previously dropped entirely by
  "fullest request wins" reconstruction.
- Bounded-memory pipeline: record bodies are now lazy (`model.Body.Source`),
  the claude fetcher hands out file-backed references instead of retaining
  every transcript, conversation grouping reads no bodies at all, Anthropic
  reconstruction parses only the fullest request per conversation
  (count-first), and `--format ir` streams conversations to disk one at a
  time. Measured on a real capture: a 7.6k-invocation day exports its IR with
  ~200 MB peak memory (previously ~15.7 GB for a smaller day), and the entire
  25 GB / 112k-file capture exports in a single run with ~700 MB peak.
- New persistent `--raw-bodies` flag (`omit` default, `embed`) controlling
  verbatim request/response payload embedding in the IR, uniformly across all
  providers.

- Machine-readable Intermediate Representation (IR) export, a second output
  sink alongside the HTML report. Selected with the new persistent `--format`
  flag (`html` default, `ir`, or `both`) and `--ir-dir`, it writes a directory
  of one JSON file per conversation plus a `manifest.json` index. Each
  conversation file carries both the reconstructed conversation (system prompt,
  tools with full descriptions and input schemas, turns of typed blocks with
  per-turn metrics) and the raw per-invocation request/response pairs with the
  provider JSON bodies embedded inline, so a downstream analysis program can
  consume conversations at full fidelity without re-fetching or re-parsing
  provider logs. Conversations carry a stable, run-order-independent id; output
  is deterministic. The schema is versioned (`dressage.ir/1.0`) and documented
  as a consumer contract in [docs/ir-format.md](docs/ir-format.md). To support
  faithful export the internal model now carries full (untruncated) tool
  descriptions and input schemas — tool-description truncation moved to the HTML
  render layer, leaving the report unchanged — and a first-class `media` content
  block for image/file parts (replacing the previous Gemini text placeholders).
- Google Vertex AI / Gemini log ingestion from BigQuery (the new
  `dressage vertex` subcommand). Queries a request-response logging table,
  normalizes rows into provider-neutral records, and reconstructs Gemini
  conversations from the `contents[]`/`parts[]` envelope — mapping
  `functionCall`/`functionResponse` to tool use/results, `thought` parts to
  thinking blocks, and aggregating streamed response chunks. Claude-on-Vertex
  rows contribute to summary stats but are not yet reconstructed (deferred to
  #4). The logging schema has no per-row caller identity, and Gemini exposes no
  cache-write counter; both gaps are documented. See
  [docs/providers/vertex.md](docs/providers/vertex.md).
- Azure OpenAI log ingestion via Azure Monitor Log Analytics (the new
  `dressage azure` subcommand). Queries the `AzureDiagnostics` table for the
  `RequestResponse` category, reconstructs OpenAI Chat Completions
  conversations (including `tool_calls`, the legacy `function_call`, and
  streaming-summarized responses), and renders them through the shared report
  pipeline. See [docs/providers/azure.md](docs/providers/azure.md).
- Azure OpenAI log ingestion from an Azure **Storage account** (the new
  `dressage azure-storage` subcommand), for diagnostic settings that export the
  `RequestResponse` category to a storage account instead of (or alongside) a
  Log Analytics workspace. Lists the `insights-logs-requestresponse` container,
  parses the hourly `PT1H.json` resource-log NDJSON blobs, and normalizes them
  identically to the workspace path (same `Provider: "azure"`, shared
  payload/identity decoding), so reports are sink-agnostic. Requires
  **Storage Blob Data Reader** on the account. See
  [docs/providers/azure.md](docs/providers/azure.md#storage-account-destination).
- Provider abstraction so log sources other than AWS Bedrock can be added
  without forking the codebase. Every fetcher now emits a provider-neutral
  `model.Record` (with `Identity`/`Body`) and implements the new
  `fetch.Fetcher` interface; conversation reconstruction dispatches on a
  provider envelope family. This is the groundwork for upcoming providers
  (Azure OpenAI, Vertex AI / Gemini).
- Per-provider documentation under `docs/providers/` (Bedrock, Azure, and
  Vertex guides) and a "Supported providers" table in the README.

### Changed

- **IR raw request/response bodies are omitted by default** (schema
  `dressage.ir/1.1`, additive: the body `json` fields were always optional).
  For resend-style captures embedded bodies grow quadratically with
  conversation length — a single busy day produced a multi-GB IR. The
  manifest's new `raw_bodies` field records `"embedded"` or `"omitted"`;
  consumers needing exact wire payloads must check it and export with
  `--raw-bodies embed`. Token/cache accounting, the reconstructed view, and
  the Parquet tables are unaffected by the default.
- Body payloads for the `claude` provider are read at export time rather than
  snapshot at fetch: a capture file deleted mid-run degrades that one body
  (logged, marker rendered) instead of being preserved by virtue of eager
  loading.
- **BREAKING (CLI):** Bedrock analysis now lives under a `bedrock` subcommand.
  The flat `dressage --bucket ...` invocation is replaced by
  `dressage bedrock --bucket ...`. The `--bucket`, `--prefix`, `--region`,
  and `--profile` flags are local to the `bedrock` subcommand.
- **BREAKING (CLI):** `--start`, `--end`, and `--output` are now persistent
  root flags shared across providers; they may be given before or after the
  subcommand. Running `dressage` with no subcommand prints help.
