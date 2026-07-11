# Proposal: Dressage as a Conversation IR Exporter

**Status:** Draft / proposal
**Author:** (drafted with Claude)
**Date:** 2026-06-01
**Audience:** Dressage maintainers; authors of the future analysis program(s).

## Summary

Dressage today ingests hosted-LLM invocation logs from three providers (AWS
Bedrock, Azure OpenAI, Google Vertex AI), normalizes them into provider-neutral
records, groups them into conversations, and renders a single self-contained
HTML report. The HTML is for humans.

This proposal adds a second output: a machine-readable **Intermediate
Representation (IR)** of the same normalized data, so that a *separate* future
program can consume conversations and analyze them (judge productivity, classify
outcomes, extract structured signals, etc.). Dressage stays focused on
**ingest + normalize + export**; all analysis lives downstream.

Concretely, this proposal:

1. Adds an `internal/ir` package that serializes the existing
   `summary.Summarize()` output into a stable, versioned JSON schema.
2. Writes the IR as a **directory of one JSON file per conversation plus a
   `manifest.json` index** (decision below).
3. Includes, per conversation, **both** the reconstructed conversation (turns /
   blocks / metrics) **and** the raw per-invocation request/response pairs
   (decision below).
4. **Enriches the internal model** so the IR is a faithful ground-truth export
   rather than a display-oriented, lossy one (decision below).
5. Exposes export via an **output-format flag** on the existing provider
   subcommands (decision below) — no new provider plumbing.

## Background: the current pipeline

```
provider subcommand (bedrock | azure | azure-storage | vertex)
        │  builds a fetch.Fetcher
        ▼
runReport(ctx, fetcher, title, common)            cmd/dressage/main.go
        │  fetcher.Fetch(ctx, start, end) -> []model.Record
        ▼
summary.Summarize(records) -> *model.Report       internal/summary
        │  - groups records by UTC day
        │  - groups into conversations (session id, else (provider,model,
        │    principal)+5min gap)
        │  - conversation.Reconstruct(records) -> *model.ConversationDetail
        ▼
report.Generate(report, output) -> report.html    internal/report
```

The data the exporter needs **already exists** at the `*model.Report` stage:

- `model.Record` — the normalized invocation (provider, timestamp, request id,
  model id, operation, status, error, `Identity`, `Input`/`Output` `Body` with
  raw JSON + token/cache counts, `LatencyMs`, `ProviderExtras`).
- `model.ConversationSummary` — per-conversation grouping with stats,
  `[]Invocation` (display copies of each request/response), and a reconstructed
  `*ConversationDetail`.
- `model.ConversationDetail` — `SystemPrompt`, `[]ToolDef`, `[]Turn`; each
  `Turn` has `[]ContentBlock` and (for assistant turns) `*TurnMetrics`.

So the IR exporter is a new **sink** that branches off the same
`*model.Report`, parallel to `report.Generate`. It does **not** require touching
any fetcher.

> Note: `CLAUDE.md` predates the multi-provider refactor and still describes a
> Bedrock-only `internal/model/log.go`/`s3fetch`. The current source of truth is
> `internal/model/record.go`, `internal/model/conversation.go`,
> `internal/fetch`, and the per-provider fetch packages. This proposal targets
> the current code.

## Goals

- Emit a **stable, versioned, provider-neutral** JSON representation of
  conversations and their constituent request/response pairs.
- Be a **faithful** record: a downstream judge/extractor should not have to
  re-fetch or re-parse provider-native logs to get full fidelity.
- Reuse the existing fetch → normalize → group → reconstruct pipeline verbatim;
  add only a new sink.
- Make the IR easy to consume from non-Go tooling (Python LLM harnesses, batch
  jobs) — predictable field names, one conversation per file, a manifest index.
- Keep the HTML report byte-for-byte unchanged (golden test still passes).

## Non-goals

- The analysis/judging/classification program itself. The IR is the contract;
  consumers are out of scope and will live in a separate repo/binary.
- Redaction / PII scrubbing of prompt content (see *Open questions*). v1 exports
  payloads verbatim, exactly as the HTML report already does.
- A streaming/append IR format for unbounded log volumes. Like the rest of
  Dressage, the exporter operates on an in-memory `*model.Report`.
- Backward-compatibility guarantees *before* v1 of the schema is tagged.

## Design decisions

These four forks were settled with the maintainer; recorded here with rationale.

### D1 — Trigger: an output-format flag on existing subcommands

Add a persistent `--format` flag (and an IR destination flag) to the shared
`commonFlags`, consumed in `runReport`. No new top-level verb, no per-provider
duplication.

```
--format   html | ir | both     (default: html)
--ir-dir   <dir>                 (default: derived from --output, see below)
```

- `--format html` (default): today's behaviour, writes `--output`
  (`report.html`).
- `--format ir`: writes only the IR directory; skips HTML.
- `--format both`: writes HTML **and** the IR directory from one fetch.

`--ir-dir` defaults to the `--output` path with its extension replaced by
`.ir/` (e.g. `report.html` → `report.ir/`), so a bare `--format both` needs no
extra flags. A single fetch can therefore produce both artifacts, which is the
main reason for a flag over a separate `dressage export …` verb: ingestion
(S3/BigQuery/Log Analytics calls) is the expensive part and should run once.

*Rejected alternatives:* a separate `export` subcommand (duplicates every
provider's flag wiring); always emitting an IR sidecar (no way to opt out, and
forces the directory write on every HTML run).

### D2 — Contents: reconstructed conversation **and** raw invocations

Each conversation IR carries two layers:

- **`conversation`** — the normalized, reconstructed view (`system_prompt`,
  `tools`, `turns[]` of typed `blocks[]`, per-turn `metrics`). This is what a
  judge reads to understand "what happened."
- **`invocations[]`** — every underlying request/response pair as a normalized
  record, **including the raw provider JSON bodies**. This is ground truth: an
  extractor that needs the exact wire payload (full tool schema, an unmodelled
  field, the literal streamed chunks) reads here.

Carrying both has three concrete payoffs:

1. The reconstructor only expands the *single fullest* invocation into turns
   (see `reconstructAnthropic`/`reconstructGemini`: it picks the request with
   the most messages and appends the final response). Per-invocation prompts,
   retries, and intermediate states are only visible in `invocations[]`.
2. Providers whose conversation reconstruction is deferred (non-Gemini Vertex /
   Claude-on-Vertex, gated by `familyVertexDeferred` in `dispatch.go`) still
   produce a useful IR: `conversation` is `null`, but `invocations[]` is fully
   populated. The IR degrades gracefully instead of emitting nothing.
3. It future-proofs the schema: new reconstruction logic can be added later
   without re-exporting, because the raw bodies were preserved.

*Rejected alternatives:* conversation-only (loses ground truth, useless for
deferred providers); raw-only (pushes turn assembly — already solved in
`internal/conversation` — onto every downstream consumer).

### D3 — Fidelity: enrich the internal model for faithful export

The current internal model is shaped for HTML display and is **lossy** in three
ways that matter to a programmatic consumer:

| Lossy behaviour today | Location | Fix for faithful IR |
|---|---|---|
| Tool descriptions truncated to 200 chars | `extractTools`, `openaiTools`, `geminiTools` | Stop truncating in reconstruction; carry full text. |
| Tool **input schema dropped entirely** (only name + description parsed) | `apiTool`, `openaiTool.Function`, `geminiFunctionDecl` | Add `InputSchema json.RawMessage` to `model.ToolDef`; parse `input_schema` (Anthropic) / `function.parameters` (OpenAI) / `functionDeclarations[].parameters` (Gemini). |
| Images/files surfaced as text placeholders like `[inline data: image/png]` | `partsToBlocks` (Gemini), and the planned-but-absent `media` block type noted in `conversation.go` | Add a first-class `media` `ContentBlock` carrying `mime_type` / `file_uri`. |

The IR is meant to *replace* re-parsing provider logs, so it must not bake in
display shortcuts. Two supporting principles:

- **Move truncation to render time.** Truncation is a presentation concern.
  Relocate it from reconstruction into the HTML template layer (the report
  already owns a `truncate` template func). The internal model then holds full
  data; HTML output is unchanged; the IR gets the full text for free.
- **Raw bodies are the backstop.** Even after enrichment, D2's `invocations[]`
  guarantees that anything the normalized view still doesn't model (e.g. raw
  base64 media bytes, which the IR references by metadata rather than inlining)
  remains recoverable from the raw payload.

*Rejected alternatives:* serialize the current model as-is (ships truncated tool
docs and placeholder media — actively misleading to an automated consumer);
lossy-view-with-authoritative-raw-only (acceptable fallback, but leaves the
clean view that judges actually read degraded when the fix is small and local).

### D4 — Layout: a directory of per-conversation files + a manifest

```
report.ir/
├── manifest.json                         # run metadata + index of all conversations
└── conversations/
    ├── <conversation-id>.json            # one conversation IR per file
    ├── <conversation-id>.json
    └── …
```

- **`manifest.json`** holds run-level metadata (tool version, source provider,
  date range, filters, generation time), aggregate stats, and a lightweight
  **index** entry per conversation (id, file path, model, session id, time
  bounds, turn/invocation counts, token totals) — enough for a consumer to
  triage and shard without opening every file.
- **`conversations/<id>.json`** is a complete, self-contained conversation IR.

This layout suits the downstream workflow: a batch judge can fan out over the
manifest, hand one conversation file to one worker, and write results keyed by
the same id. Individual conversations are trivial to inspect, diff, or
re-process.

*Rejected alternatives:* one big JSON document (forces whole-report reads,
awkward to shard, large); JSONL one-conversation-per-line (streams well but is
harder to inspect/diff a single conversation, and a partial write corrupts the
whole file). A consumer that *wants* JSONL can trivially `cat` the per-file IRs
together.

## IR schema (v1)

JSON, **snake_case** field names (idiomatic for the cross-language consumers we
expect). Every file embeds `schema_version`. Raw provider bodies are embedded as
**inline JSON** (not stringified), so a conversation file is itself valid JSON
and a consumer can walk into a request body directly.

### `manifest.json`

```jsonc
{
  "schema_version": "dressage.ir/1.0",
  "generated_at": "2026-06-01T12:00:00Z",
  "tool": { "name": "dressage", "version": "v1.4.0" },
  "source": {
    "provider": "bedrock",            // dominant provider in this run
    "command": "dressage bedrock --bucket my-logs --start 2026-05-01 …",
    "date_range": { "start": "2026-05-01", "end": "2026-05-31" }
  },
  "totals": {
    "conversations": 128,
    "invocations": 4213,
    "input_tokens": 91003241,
    "output_tokens": 5120093,
    "errors": 7
  },
  "conversations": [
    {
      "id": "b1c2…",                  // stable id (see Determinism)
      "file": "conversations/b1c2….json",
      "provider": "bedrock",
      "model_id": "eu.anthropic.claude-opus-4-6-v1",
      "session_id": "f47ac10b-58cc-4372-a567-0e02b2c3d479",
      "start_time": "2026-05-03T09:14:02Z",
      "end_time": "2026-05-03T09:41:55Z",
      "turn_count": 38,
      "invocation_count": 19,
      "input_tokens": 882104,
      "output_tokens": 41233,
      "error_count": 0,
      "reconstructed": true           // false for deferred providers
    }
    // …
  ]
}
```

### `conversations/<id>.json`

```jsonc
{
  "schema_version": "dressage.ir/1.0",
  "id": "b1c2…",
  "session_id": "f47ac10b-…",
  "provider": "bedrock",
  "model_id": "eu.anthropic.claude-opus-4-6-v1",
  "identity": {
    "principal": "arn:aws:sts::123456789012:assumed-role/…",
    "display": "harness-ci",
    "extra": { "accountId": "123456789012", "region": "eu-west-1" }
  },
  "start_time": "2026-05-03T09:14:02Z",
  "end_time": "2026-05-03T09:41:55Z",
  "stats": {
    "invocation_count": 19,
    "input_tokens": 882104,
    "output_tokens": 41233,
    "cache_read_tokens": 781002,
    "cache_write_tokens": 96000,
    "error_count": 0
  },

  // ── Layer 1: reconstructed conversation (null for deferred providers) ──
  "conversation": {
    "system_prompt": "You are a coding assistant…",   // full, untruncated
    "tools": [
      {
        "name": "Bash",
        "description": "Executes a bash command…",     // full, untruncated
        "input_schema": { "type": "object", "properties": { … } }  // inline JSON
      }
    ],
    "turns": [
      {
        "role": "user",
        "blocks": [
          { "type": "text", "text": "Fix the failing test in foo_test.go" }
        ]
      },
      {
        "role": "assistant",
        "blocks": [
          { "type": "thinking", "text": "The test asserts…" },
          { "type": "text", "text": "I'll inspect the test first." },
          {
            "type": "tool_use",
            "tool_id": "toolu_01A…",
            "tool_name": "Bash",
            "tool_input": { "command": "go test ./..." }   // inline JSON
          },
          {
            "type": "media",
            "mime_type": "image/png",
            "file_uri": "gs://bucket/screenshot.png"        // file_uri xor inline
          }
        ],
        "metrics": {
          "timestamp": "2026-05-03T09:14:09Z",
          "request_id": "abc-123",
          "model_id": "eu.anthropic.claude-opus-4-6-v1",
          "input_tokens": 46201,
          "output_tokens": 1820,
          "cache_read_tokens": 44000,
          "cache_write_tokens": 0,
          "latency_ms": 4120,
          "first_byte_ms": 510,
          "stop_reason": "tool_use"
        }
      },
      {
        "role": "user",
        "blocks": [
          {
            "type": "tool_result",
            "tool_id": "toolu_01A…",
            "is_error": false,
            "result_content": "ok  …  0.412s"
          }
        ]
      }
    ]
  },

  // ── Layer 2: raw invocations (ground truth) ──
  "invocations": [
    {
      "timestamp": "2026-05-03T09:14:02Z",
      "request_id": "abc-123",
      "model_id": "eu.anthropic.claude-opus-4-6-v1",
      "operation": "InvokeModelWithResponseStream",
      "status": "200",
      "error_code": "",
      "identity": { "principal": "arn:aws:sts::…", "display": "harness-ci" },
      "latency_ms": 4120,
      "input": {
        "content_type": "application/json",
        "token_count": 46201,
        "cache_read": 44000,
        "cache_write": 0,
        "json": { "messages": [ … ], "system": [ … ], "tools": [ … ] }  // inline, verbatim
      },
      "output": {
        "content_type": "application/json",
        "token_count": 1820,
        "cache_read": 0,
        "cache_write": 0,
        "json": [ { "type": "message_start", … }, … ]   // inline, verbatim (streamed chunks as array)
      },
      "provider_extras": { … }       // model.Record.ProviderExtras, if present
    }
    // … one per request/response pair in the conversation
  ]
}
```

### Block-type taxonomy

`blocks[].type` is an **open** set (the code already warns readers not to treat
it as closed). v1 defines: `text`, `thinking`, `tool_use`, `tool_result`,
`media`. Unrecognized provider block types pass through with their provider type
string and a best-effort `text` payload. Field presence by type:

| type | fields |
|---|---|
| `text` | `text` |
| `thinking` | `text` |
| `tool_use` | `tool_id`, `tool_name`, `tool_input` (inline JSON) |
| `tool_result` | `tool_id`, `is_error`, `result_content` |
| `media` *(new)* | `mime_type`, and one of `file_uri` (external) or `inline: true` + `byte_size` (bytes themselves remain in the raw body) |

### Schema versioning

- `schema_version` is `"dressage.ir/MAJOR.MINOR"`. Additive, backward-compatible
  changes bump MINOR; breaking changes bump MAJOR. Consumers should accept any
  matching MAJOR and ignore unknown fields.
- The version is a single source-of-truth constant in `internal/ir`.

## Code changes

### New package `internal/ir`

```
internal/ir/
  ir.go        // IR types (Manifest, ManifestEntry, ConversationIR, InvocationIR,
               //  BodyIR, IdentityIR, ConversationView, TurnIR, BlockIR, MetricsIR, ToolIR)
               //  with json tags; SchemaVersion constant.
  export.go    // Export(report *model.Report, dir string, src SourceInfo) error
               //  - mkdir dir + dir/conversations
               //  - walk report.Days[].Conversations[]
               //  - map each to ConversationIR, write conversations/<id>.json
               //  - accumulate manifest entries, write manifest.json
  map.go       // pure mapping functions model.* -> ir.* (unit-testable, no IO)
  ir_test.go   // golden tests (mirrors report/golden_test.go), round-trip, determinism
```

`Export` mirrors `report.Generate`'s role: it takes the same `*model.Report`
and a destination, and is the only IO-touching function. All `model.* → ir.*`
translation is pure for testability.

### Internal model enrichment (D3)

- `model.ToolDef`: add `InputSchema json.RawMessage`. Populate it in the three
  reconstructors; **stop** truncating descriptions there.
- `model.ContentBlock`: add a `media` type plus `MimeType`, `FileURI`,
  `MediaInline bool`, `MediaBytes int64` fields (metadata only; bytes stay in
  the raw body). Update `partsToBlocks` (Gemini) to emit `media` blocks instead
  of text placeholders; wire the Anthropic/OpenAI image parts similarly.
- `internal/report/template.html`: apply `truncate` at render time for tool
  descriptions (so HTML output is unchanged) and render `media` blocks (a small
  labeled element replacing today's inline-data placeholder text). The golden
  test pins this.

These are small, local changes; the parsers already decode the surrounding
structures.

### CLI wiring (D1)

In `cmd/dressage/main.go`:

- Extend `commonFlags` with `format string` and `irDir string`; register
  `--format` (default `html`) and `--ir-dir` on the root persistent flags.
- In `runReport`, after `summary.Summarize`, branch on `format`:
  - `html` / `both` → `report.Generate(rpt, common.output)` (unchanged).
  - `ir` / `both` → resolve `irDir` (default: `--output` with extension swapped
    to `.ir/`), build `ir.SourceInfo` from the active subcommand + flags, call
    `ir.Export(rpt, irDir, src)`.
  - Validate `format` ∈ {html, ir, both}; error clearly otherwise.
- Extend the stdout summary block to report the IR directory and conversation
  file count when IR was written.

No fetcher or provider package changes.

## Determinism & stable IDs

The current conversation id (`conv-YYYYMMDD-N`, a per-run global counter in
`summary.go`) is **order-dependent** and not stable across runs if the input
window shifts — bad for a downstream store that re-imports or references
conversations over time.

For the IR, derive a **stable id**:

1. If a session id is present, use it (it is already stable and provider-unique
   via the `(provider, session)` key).
2. Otherwise, hash `(provider, model_id, principal, start_time_RFC3339)` (e.g.
   first 16 hex chars of SHA-256) — deterministic for the same underlying
   conversation regardless of run order or sibling conversations.

The internal `conv-…` id can be retained as a secondary `display_id` field for
cross-referencing the HTML report. File names derive from the stable id;
manifest `conversations[]` is sorted by `start_time` then id for byte-stable
output (enabling golden tests and clean diffs).

## Testing strategy

- **Golden IR fixtures**, mirroring `internal/report/golden_test.go`: feed a
  canned `*model.Report` (or canned provider log fixtures end-to-end) through
  `ir.Export` into a temp dir and compare every emitted file against checked-in
  goldens. Covers schema shape, determinism, and stable-id derivation.
- **Round-trip**: unmarshal each emitted file back into the `ir` types and
  assert structural equality, guaranteeing the IR is valid, self-describing JSON.
- **Per-provider coverage**: at least one fixture each for Bedrock (Anthropic),
  Azure (OpenAI), Vertex/Gemini, and a **deferred** provider (assert
  `conversation == null` but `invocations[]` populated).
- **Fidelity assertions**: a tool with a >200-char description and a non-trivial
  input schema survives untruncated and with schema intact; a Gemini
  `inlineData`/`fileData` part becomes a `media` block.
- **HTML unchanged**: the existing report golden test must still pass after the
  enrichment + render-time-truncation refactor.

## Implementation plan

Phased so each step is independently reviewable and the HTML path never breaks.

1. **Model enrichment + render-time truncation** (D3 groundwork). Add
   `ToolDef.InputSchema`, the `media` block fields; move truncation into the
   template; update the three reconstructors. *Gate:* HTML golden test
   unchanged; new unit tests for full tool schema + media blocks.
2. **`internal/ir` types + pure mappers** (`ir.go`, `map.go`) with unit tests on
   the `model.* → ir.*` mapping, including stable-id derivation.
3. **`ir.Export` + manifest writer** (`export.go`) with golden + round-trip
   tests across all four provider families.
4. **CLI wiring** (`--format`, `--ir-dir`, stdout summary) and an end-to-end
   smoke test.
5. **Docs**: a `docs/ir-format.md` schema reference (the normative contract for
   downstream authors), README "Outputs" section, and a CHANGELOG entry.

Steps 1–4 are mergeable in sequence behind the default (`--format html`), so the
feature can land incrementally without affecting existing users.

## Open questions / future work

- **Redaction / PII.** The IR contains full prompts and tool I/O verbatim, the
  same content the HTML already exposes, but now in a form that flows easily into
  other systems. A future `--redact` mode (e.g. drop/blur tool_result bodies,
  hash identities) may be warranted before IR is shared outside the analyst's
  machine. v1 documents the sensitivity and exports verbatim.
- **Media bytes.** v1 keeps inline media bytes only in the raw body and
  references them by metadata in the `media` block. If a consumer needs decoded
  media, a future option could extract bytes to sidecar files under the
  conversation dir.
- **Analysis-result schema.** The downstream judge/classifier will itself emit
  structured verdicts. A companion "analysis IR" (keyed by the same stable
  conversation ids) is the natural next contract, but is explicitly out of scope
  here.
- **Cross-run aggregation.** Whether a future mode should merge multiple runs'
  manifests (dedup by stable id) for longitudinal analysis.
- **Compression.** Large runs may want gzipped conversation files; deferred
  until volume warrants it.

## Appendix: end-to-end example

```bash
# Fetch once, emit both the human report and the machine IR.
dressage bedrock \
  --bucket my-bedrock-logs \
  --start 2026-05-01 --end 2026-05-31 \
  --format both \
  --output may-report.html        # IR defaults to ./may-report.ir/

# Result:
#   may-report.html
#   may-report.ir/manifest.json
#   may-report.ir/conversations/<id>.json   (one per conversation)

# A downstream judge then fans out over the manifest:
jq -r '.conversations[].file' may-report.ir/manifest.json \
  | xargs -P8 -I{} my-judge --conversation may-report.ir/{}
```
