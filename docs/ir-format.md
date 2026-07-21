# Dressage IR format (v1)

This is the normative reference for the Dressage **Intermediate Representation
(IR)**: the machine-readable export a downstream analysis program consumes
instead of re-fetching or re-parsing provider-native logs. It is the contract
for consumers; if this document and the emitted output ever disagree, that is a
bug in Dressage.

It is the sole output of an ingestion run, written to `--out` (default
`report.ir`); browse it with `dressage serve <ir-dir>` — see the
[README](../README.md#outputs).

## Layout

The IR is a directory:

```
report.ir/
├── manifest.json                    # run metadata + index of all conversations
├── facts.parquet                    # columnar per-invocation facts table
├── turns.parquet                    # columnar deduplicated-turns table
└── conversations/
    ├── <id>.json                    # one self-contained conversation IR per file
    ├── <id>.json
    └── …
```

- `manifest.json` carries run-level metadata, aggregate totals, and a
  lightweight index entry per conversation — enough to triage and shard without
  opening every file.
- `conversations/<id>.json` is a complete, self-contained conversation IR. The
  file name is a filesystem-safe transform of the conversation's stable `id`
  (see [Stable conversation id](#stable-conversation-id)); always resolve a
  conversation's file via the manifest `file` field rather than rebuilding the
  path from `id`.
- `facts.parquet` and `turns.parquet` are zstd-compressed Parquet tables for
  analytical engines (e.g. DuckDB — see the
  [cookbook](duckdb-cookbook.md)): one row per invocation and one row per
  deduplicated conversation turn respectively. Resolve them via the manifest
  `files` field, never by hard-coding names.

This suits a fan-out workflow: a batch judge reads the manifest, hands one
conversation file to one worker, and writes results keyed by the same `id` —
while aggregate questions (spend, cache hit rates, session shapes, tool
frequency, full-text search) run against the two Parquet tables without
opening any JSON.

## Conventions

- **Encoding.** UTF-8 JSON, indented two spaces, one trailing newline per file.
- **Field names.** `snake_case` throughout.
- **Timestamps.** RFC 3339 / ISO 8601 in UTC (e.g. `2026-05-03T09:14:02Z`).
- **Raw bodies are opt-in inline JSON.** When embedded (see
  [`raw_bodies`](#raw-bodies-are-opt-in)), provider request/response bodies and
  tool input/parameters schemas embed as JSON values, never as escaped strings,
  so a conversation file is itself valid JSON and a consumer can walk directly
  into a request body.
- **Omitted fields.** Fields documented as optional are omitted when empty
  (Go `omitempty`); a consumer must treat an absent field as its zero value.
  `conversation` is the sole exception: it is always present, and is explicitly
  `null` when the conversation was not reconstructed (see below).
- **Determinism.** For a given report the JSON output is byte-stable across
  runs: the manifest index is sorted by `start_time` then `id`, and file names
  derive deterministically from the `id` via the filesystem-safe transform
  below. The Parquet tables are **content**-deterministic (same rows in the
  same order) but not byte-stable across dressage releases, because the file
  footer embeds the writing library's version.

## Schema version

Every file embeds `schema_version`, a string of the form
`"dressage.ir/MAJOR.MINOR"`. This document describes `dressage.ir/1.3`.

- Additive, backward-compatible changes (new optional fields, new block types)
  bump **MINOR**.
- Breaking changes (renamed/removed fields, changed semantics) bump **MAJOR**.
- Consumers should accept any matching **MAJOR** and **ignore unknown fields**.

Version history:

- **1.3** — the manifest's `totals` and each `conversations[]` index entry
  gained `cache_read_tokens` / `cache_write_tokens`, mirroring the
  per-conversation `stats` block, so cache accounting is available at every
  aggregation level without opening conversation files. Additive.
- **1.2** — reconstructed **sidechains** (subagent threads) are now carried in
  each conversation file (`conversation.sidechains[]`), so a consumer renders
  subagents without joining `turns.parquet`; the manifest's `totals` gained
  `model_breakdown` / `op_breakdown` maps; and each `conversations[]` index entry
  gained `display_id`. All additive.
- **1.1** — raw request/response bodies became **opt-in** (the manifest's
  `raw_bodies` field records the choice; the body `json` fields were always
  optional); added the `files` manifest field and the `facts.parquet` /
  `turns.parquet` tables.
- **1.0** — initial schema.

## Raw bodies are opt-in

By default an export **omits** the verbatim request/response payloads
(`invocations[].input.json` / `output.json`): for resend-style captures (the
`claude` provider, where the client re-sends the whole running transcript on
every turn) embedded bodies grow quadratically with conversation length —
multi-GB IR directories for a single day. Token/cache accounting on every
invocation, the reconstructed `conversation` view, and both Parquet tables are
unaffected; the deduplicated turns (main thread **and** sidechains) carry the
conversation content.

Pass `--raw-bodies embed` to restore verbatim embedding (uniformly, for every
provider). The manifest records the mode in `raw_bodies` (`"embedded"` |
`"omitted"`); a consumer that needs exact wire payloads must check it before
relying on the body `json` fields.

## Stable conversation id

Each conversation has a stable `id` that does not depend on run order or on the
input window, so a store can re-import or cross-reference conversations over
time:

1. If a session id is present (extracted from the provider's request body), the
   `id` **is** that session id.
2. Otherwise the `id` is the first **16 hex characters** of the SHA-256 of
   `provider`, `model_id`, `principal`, and `start_time` (RFC 3339), joined with
   NUL byte separators.

Each conversation also carries `display_id`, a human-friendly `conv-YYYYMMDD-N`
label (day plus run-order index). `display_id` is **run-order dependent** and
must not be used as a stable key; it exists only as a readable handle in
conversation listings.

**Filenames.** A session id can contain path-significant characters (`/`, `\`,
`:`) because it comes from user-controlled request fields. The `id` field in the
content always keeps the raw value, but the on-disk filename and the manifest
`file` path apply a filesystem-safe transform: those characters are replaced
with `_`, any residual path component is stripped, an empty result falls back to
a content hash, and distinct ids that collapse to the same name are
disambiguated with a `_2`, `_3`, … suffix. Always locate a conversation file via
the manifest `file` field rather than constructing `conversations/<id>.json`
yourself.

## `manifest.json`

| Field | Type | Notes |
|---|---|---|
| `schema_version` | string | `"dressage.ir/1.3"`. |
| `generated_at` | timestamp | When the report was produced. |
| `tool` | object | `{ "name": "dressage", "version": "<build version>" }`. |
| `source` | object | Run provenance (below). |
| `raw_bodies` | string | `"embedded"` or `"omitted"` — whether invocation payload `json` fields are present (see [Raw bodies are opt-in](#raw-bodies-are-opt-in)). |
| `files` | object | Run-level sibling artifacts: `{ "facts": "facts.parquet", "turns": "turns.parquet" }`. Resolve tables through this field. |
| `totals` | object | Report-wide aggregates (below). |
| `conversations` | array | Index entries, sorted by `start_time` then `id`. |

`source`:

| Field | Type | Notes |
|---|---|---|
| `provider` | string | Dominant provider in this run (`bedrock`, `azure`, `vertex`, `claude`). |
| `command` | string | The command line that produced the run. Values of sensitive flags (`--credentials`, `--profile`, `--subscription`, `--workspace`, `--tenant`, `--account`) are replaced with `<redacted>`. |
| `date_range` | object | `{ "start": "YYYY-MM-DD", "end": "YYYY-MM-DD" }`; either may be empty for an unbounded edge. |

`totals`:

| Field | Type | Notes |
|---|---|---|
| `conversations` | integer | |
| `invocations` | integer | |
| `input_tokens` | integer | |
| `output_tokens` | integer | |
| `cache_read_tokens` | integer | Summed per each provider's own accounting: Anthropic/Bedrock report cache tokens alongside `input_tokens`; OpenAI/Gemini report cache reads as a subset of the prompt count. |
| `cache_write_tokens` | integer | `0` for providers without a cache-write counter (OpenAI, Gemini). |
| `errors` | integer | |
| `model_breakdown` | object | Map of `model_id` → invocation count across the run. Always a concrete object (`{}` for an empty run). |
| `op_breakdown` | object | Map of operation name → invocation count across the run. Always a concrete object. |

`conversations[]` (index entry):

| Field | Type | Notes |
|---|---|---|
| `id` | string | Stable id (raw, may contain `/` etc. when it is a session id). |
| `display_id` | string | Human-friendly `conv-YYYYMMDD-N` label (run-order dependent; not a stable key). |
| `file` | string | Path relative to the IR directory, e.g. `conversations/<name>.json`, where `<name>` is the id passed through a filesystem-safe transform (see [Stable conversation id](#stable-conversation-id)). |
| `provider` | string | |
| `model_id` | string | |
| `session_id` | string | Omitted when no session id was found. |
| `start_time` | timestamp | |
| `end_time` | timestamp | |
| `turn_count` | integer | Reconstructed turns; `0` when not reconstructed. |
| `invocation_count` | integer | Underlying request/response pairs. |
| `input_tokens` | integer | |
| `output_tokens` | integer | |
| `cache_read_tokens` | integer | Same semantics as the `totals` fields. |
| `cache_write_tokens` | integer | Same semantics as the `totals` fields. |
| `error_count` | integer | |
| `reconstructed` | boolean | `false` when `conversation` is `null`. |

## `conversations/<id>.json`

Top-level fields:

| Field | Type | Notes |
|---|---|---|
| `schema_version` | string | `"dressage.ir/1.3"`. |
| `id` | string | Stable id (matches the file name and the manifest entry). |
| `display_id` | string | Human-friendly `conv-…` label (run-order dependent; not a stable key). |
| `session_id` | string | Omitted when absent. |
| `provider` | string | |
| `model_id` | string | |
| `identity` | object | Principal that made the invocations (below). |
| `start_time` | timestamp | |
| `end_time` | timestamp | |
| `stats` | object | Per-conversation aggregates (below). |
| `conversation` | object \| null | Reconstructed main-thread view, or `null` when not reconstructed (see `reconstructed` and Layer 1 below). |
| `sidechains` | array | Reconstructed subagent threads (below); **omitted** when the conversation has none. |
| `invocations` | array | Raw request/response pairs; **always populated**. |

`identity`:

| Field | Type | Notes |
|---|---|---|
| `principal` | string | ARN (Bedrock), Entra OID (Azure), service-account email (Vertex). Omitted when empty. |
| `display` | string | Human-friendly label; omitted when empty. |
| `extra` | object | Provider-specific attributes (`accountId`, `region`, `subscription`, `project`, …). Omitted when empty. |

`stats`:

| Field | Type |
|---|---|
| `invocation_count` | integer |
| `input_tokens` | integer |
| `output_tokens` | integer |
| `cache_read_tokens` | integer |
| `cache_write_tokens` | integer |
| `error_count` | integer |

### Layer 1 — `conversation` (reconstructed view)

The normalized, reconstructed view a judge reads to understand "what happened".
It is `null` when reconstruction was not performed — in two cases: for
**deferred providers** (non-Gemini Vertex, e.g. Claude-on-Vertex), and for
conversations assembled by the **time-gap heuristic** rather than by session id
(records that carry no session id are grouped by a 5-minute gap and are not
reconstructed; a pre-existing limitation, present in the HTML report too). In
both cases `reconstructed` is `false`. When `conversation` is `null`,
`invocations` is still fully populated, so the IR degrades gracefully rather
than emitting nothing.

| Field | Type | Notes |
|---|---|---|
| `system_prompt` | string | Full, untruncated; `""` when no system prompt was present. |
| `tools` | array | Tool definitions (below); `[]` when no tools were defined. |
| `turns` | array | Ordered turns (below). |

`tools[]`:

| Field | Type | Notes |
|---|---|---|
| `name` | string | |
| `description` | string | **Full, untruncated.** |
| `input_schema` | inline JSON | The tool's input/parameters JSON schema; omitted when the provider supplied none. |

`turns[]`:

| Field | Type | Notes |
|---|---|---|
| `role` | string | `user` or `assistant`. |
| `blocks` | array | Typed content blocks (below). |
| `metrics` | object | Present on assistant turns (below); omitted otherwise. |

`metrics`:

| Field | Type | Notes |
|---|---|---|
| `timestamp` | timestamp | |
| `request_id` | string | Omitted when empty. |
| `model_id` | string | Omitted when empty. |
| `input_tokens` | integer | |
| `output_tokens` | integer | |
| `cache_read_tokens` | integer | |
| `cache_write_tokens` | integer | `0` for providers without a cache-write counter (OpenAI, Gemini). |
| `latency_ms` | integer | |
| `first_byte_ms` | integer | |
| `stop_reason` | string | Provider stop/finish reason; omitted when empty. |

### Block-type taxonomy

`blocks[].type` is an **open** set: v1 defines the types below, and unrecognized
provider block types pass through with their provider type string and a
best-effort `text` payload. A consumer must not assume the set is closed.

| `type` | Fields | Notes |
|---|---|---|
| `text` | `text` | |
| `thinking` | `text` | Model reasoning, where the provider exposes it. |
| `tool_use` | `tool_id`, `tool_name`, `tool_input` | `tool_input` is inline JSON. |
| `tool_result` | `tool_id`, `is_error`, `result_content` | `is_error` omitted when false; `result_content` omitted when empty. |
| `media` | `mime_type`, and either `file_uri` **or** `inline` + `byte_size` | See below. |

A `media` block is metadata only — the raw bytes are **not** inlined here. For
external media (e.g. a `gs://` URI) `file_uri` is set. For media embedded inline
in the request, `inline` is `true` and `byte_size` is the decoded byte length
(`0` when unknown); the bytes themselves remain in the matching raw body under
`invocations[]`. `mime_type` is set when the provider declared one.

### `sidechains` (subagent threads)

`sidechains` carries the reconstructed subagent threads spawned within the
conversation — the same threads that appear in `turns.parquet` with a non-empty
`thread_id` — so a consumer can render or analyse subagents without joining the
Parquet table. The field is **omitted** for the common single-thread
conversation; when present it is a non-empty array. The main thread is never
listed here (it is the top-level `conversation`).

| Field | Type | Notes |
|---|---|---|
| `id` | string | The thread id that groups this chain (matches `turns.parquet.thread_id` and `facts.parquet.thread_id`). |
| `conversation` | object | A full reconstructed view, same shape as the top-level `conversation` (`system_prompt`, `tools`, `turns`). Never `null` — a sidechain that failed to reconstruct is not listed. |

Only session-reconstructed conversations can carry sidechains: deferred and
time-gap conversations have a `null` `conversation` and no `sidechains`. The
sidechain turns are deduplicated reconstruction output (not raw resends), so
this roughly adds the subagents' turn content to a conversation file — bounded,
unlike embedded raw bodies.

### Layer 2 — `invocations` (ground truth)

Every underlying request/response pair with its token accounting and, **when
the export embedded raw bodies** (`--raw-bodies embed`; check the manifest's
`raw_bodies`), the raw provider JSON bodies inline and verbatim. An extractor
that needs the exact wire payload (full tool schema, an unmodelled field, the
literal streamed chunks) reads here after confirming the export embedded it.
Always populated, even when `conversation` is `null`.

`invocations[]`:

| Field | Type | Notes |
|---|---|---|
| `timestamp` | timestamp | |
| `request_id` | string | Omitted when empty. |
| `model_id` | string | |
| `operation` | string | Provider operation name; omitted when empty. |
| `status` | string | Raw provider status; omitted when empty. |
| `error_code` | string | Non-empty marks an errored invocation; omitted otherwise. |
| `identity` | object | Same shape as the conversation `identity`. |
| `latency_ms` | integer | `0` when the provider does not report it at the record level. |
| `input` | object | Request body (below). |
| `output` | object | Response body (below). |
| `provider_extras` | inline JSON | Opaque per-provider fields; omitted when absent. |

`input` / `output` (`BodyIR`):

| Field | Type | Notes |
|---|---|---|
| `content_type` | string | Omitted when empty. |
| `token_count` | integer | |
| `cache_read` | integer | |
| `cache_write` | integer | `0` for providers without a cache-write counter. |
| `json` | inline JSON | The raw body, verbatim. For streamed responses this is the array of chunks. Omitted when the export did not embed raw bodies (the default — see manifest `raw_bodies`), or when the raw body was absent or not valid JSON. |

## `facts.parquet` — per-invocation facts

One row per invocation — errored and sidechain invocations included — sorted
by (`timestamp`, `request_id`, `request_uuid`). This is the table analytical
queries scan; it never contains conversation content. Absent values are the
zero value (`''` / `0`), not SQL NULL, so filter with `<> ''` / `> 0`.

| Column | Type | Notes |
|---|---|---|
| `conversation_id` | string | Joins to the manifest `conversations[].id` and to `turns.parquet`. |
| `session_id` | string | `''` when the record carried none. |
| `provider` | string | |
| `model_id` | string | |
| `timestamp` | timestamp | |
| `request_id` | string | Provider/API request id. |
| `operation` | string | |
| `status` | string | Raw provider status; `''` for unpaired claude requests. |
| `error_code` | string | Non-empty marks an errored invocation. |
| `stop_reason` | string | Response stop/finish reason where the fetcher lifted it (currently the `claude` provider); `''` otherwise. |
| `input_tokens` / `output_tokens` | int64 | |
| `cache_read_tokens` / `cache_write_tokens` | int64 | |
| `latency_ms` | int64 | `0` when the provider does not report it. |
| `principal` | string | |
| `message_id` | string | Response message id (`claude`); `''` otherwise. |
| `prev_message_id` | string | Prior turn's response message id named by the request (`claude`). |
| `thread_id` | string | Chain the invocation belongs to (`claude`: the chain root's request uuid — the main thread and each subagent sidechain are distinct chains); `''` for providers without linkage. |
| `request_uuid` | string | Capture-assigned request id (`claude`: the request filename uuid). |
| `num_messages` | int32 | Transcript length of the request (`claude`); `0` when unknown. |
| `extras` | string | JSON object of provider-specific identity attributes (e.g. `device_id`); `''` when none. |

## `turns.parquet` — deduplicated conversation turns

One row per reconstructed turn: the **main thread and every sidechain**
(subagent) of each reconstructed conversation, written in conversation display
order. Each unique turn appears once regardless of how many times a
resend-style provider re-transmitted it — this table plus the facts table is
the scalable representation of a capture's content. Conversations that were
not reconstructed contribute no rows.

| Column | Type | Notes |
|---|---|---|
| `conversation_id` | string | Joins to the manifest and `facts.parquet`. |
| `session_id` | string | |
| `provider` | string | |
| `thread_id` | string | `''` for the main thread; the sidechain's id (matching `facts.thread_id`) for subagent threads. |
| `turn_index` | int32 | 0-based within its thread. |
| `role` | string | `user`, `assistant`, `system`. |
| `text` | string | The turn's text blocks flattened — the full-text-search target. |
| `blocks` | string | JSON array of the turn's typed blocks, in exactly the [block taxonomy](#block-type-taxonomy) shape used by the JSON files. |
| `has_metrics` | boolean | Distinguishes absent metrics from real zero values. |
| `timestamp`, `request_id`, `model_id`, `input_tokens`, `output_tokens`, `cache_read_tokens`, `cache_write_tokens`, `latency_ms`, `first_byte_ms`, `stop_reason` | — | The turn's metrics (same semantics as the JSON `metrics` object); zero values when `has_metrics` is false. |

## Sensitivity

The IR contains full prompts and tool input/output **verbatim**, in a form that
flows easily into other systems. It performs no redaction or PII scrubbing.
Treat IR directories as sensitive and scope their distribution accordingly.

## Versioning policy summary

- `dressage.ir/1.x` is additive and backward compatible within the major.
- Consumers: pin on `MAJOR`, ignore unknown fields, treat absent optional fields
  as zero values, and treat `blocks[].type` as an open set.
