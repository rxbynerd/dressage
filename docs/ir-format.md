# Dressage IR format (v1)

This is the normative reference for the Dressage **Intermediate Representation
(IR)**: the machine-readable export a downstream analysis program consumes
instead of re-fetching or re-parsing provider-native logs. It is the contract
for consumers; if this document and the emitted output ever disagree, that is a
bug in Dressage.

Produce it with `--format ir` (or `--format both` alongside the HTML report) —
see the [README](../README.md#outputs).

## Layout

The IR is a directory:

```
report.ir/
├── manifest.json                    # run metadata + index of all conversations
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

This suits a fan-out workflow: a batch judge reads the manifest, hands one
conversation file to one worker, and writes results keyed by the same `id`.

## Conventions

- **Encoding.** UTF-8 JSON, indented two spaces, one trailing newline per file.
- **Field names.** `snake_case` throughout.
- **Timestamps.** RFC 3339 / ISO 8601 in UTC (e.g. `2026-05-03T09:14:02Z`).
- **Raw bodies are inline JSON.** Provider request/response bodies and tool
  input/parameters schemas embed as JSON values, never as escaped strings, so a
  conversation file is itself valid JSON and a consumer can walk directly into a
  request body.
- **Omitted fields.** Fields documented as optional are omitted when empty
  (Go `omitempty`); a consumer must treat an absent field as its zero value.
  `conversation` is the sole exception: it is always present, and is explicitly
  `null` when the conversation was not reconstructed (see below).
- **Determinism.** For a given report the output is byte-stable across runs: the
  manifest index is sorted by `start_time` then `id`, and file names derive
  deterministically from the `id` via the filesystem-safe transform below.

## Schema version

Every file embeds `schema_version`, a string of the form
`"dressage.ir/MAJOR.MINOR"`. This document describes `dressage.ir/1.0`.

- Additive, backward-compatible changes (new optional fields, new block types)
  bump **MINOR**.
- Breaking changes (renamed/removed fields, changed semantics) bump **MAJOR**.
- Consumers should accept any matching **MAJOR** and **ignore unknown fields**.

## Stable conversation id

Each conversation has a stable `id` that does not depend on run order or on the
input window, so a store can re-import or cross-reference conversations over
time:

1. If a session id is present (extracted from the provider's request body), the
   `id` **is** that session id.
2. Otherwise the `id` is the first **16 hex characters** of the SHA-256 of
   `provider`, `model_id`, `principal`, and `start_time` (RFC 3339), joined with
   NUL byte separators.

Each conversation also carries `display_id`, the internal `conv-YYYYMMDD-N`
identifier shown in the HTML report. `display_id` is **run-order dependent** and
must not be used as a stable key; it exists only to cross-reference the report.

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
| `schema_version` | string | `"dressage.ir/1.0"`. |
| `generated_at` | timestamp | When the report was produced. |
| `tool` | object | `{ "name": "dressage", "version": "<build version>" }`. |
| `source` | object | Run provenance (below). |
| `totals` | object | Report-wide aggregates (below). |
| `conversations` | array | Index entries, sorted by `start_time` then `id`. |

`source`:

| Field | Type | Notes |
|---|---|---|
| `provider` | string | Dominant provider in this run (`bedrock`, `azure`, `vertex`). |
| `command` | string | The command line that produced the run. Values of sensitive flags (`--credentials`, `--profile`, `--subscription`, `--workspace`, `--tenant`, `--account`) are replaced with `<redacted>`. |
| `date_range` | object | `{ "start": "YYYY-MM-DD", "end": "YYYY-MM-DD" }`; either may be empty for an unbounded edge. |

`totals`:

| Field | Type |
|---|---|
| `conversations` | integer |
| `invocations` | integer |
| `input_tokens` | integer |
| `output_tokens` | integer |
| `errors` | integer |

`conversations[]` (index entry):

| Field | Type | Notes |
|---|---|---|
| `id` | string | Stable id (raw, may contain `/` etc. when it is a session id). |
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
| `error_count` | integer | |
| `reconstructed` | boolean | `false` when `conversation` is `null`. |

## `conversations/<id>.json`

Top-level fields:

| Field | Type | Notes |
|---|---|---|
| `schema_version` | string | `"dressage.ir/1.0"`. |
| `id` | string | Stable id (matches the file name and the manifest entry). |
| `display_id` | string | Internal `conv-…` id for cross-referencing the HTML report. |
| `session_id` | string | Omitted when absent. |
| `provider` | string | |
| `model_id` | string | |
| `identity` | object | Principal that made the invocations (below). |
| `start_time` | timestamp | |
| `end_time` | timestamp | |
| `stats` | object | Per-conversation aggregates (below). |
| `conversation` | object \| null | Reconstructed view, or `null` when not reconstructed (see `reconstructed` and Layer 1 below). |
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

### Layer 2 — `invocations` (ground truth)

Every underlying request/response pair, including the **raw provider JSON bodies
embedded inline and verbatim**. An extractor that needs the exact wire payload
(full tool schema, an unmodelled field, the literal streamed chunks) reads here.
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
| `json` | inline JSON | The raw body, verbatim. For streamed responses this is the array of chunks. Omitted when the raw body was absent or not valid JSON. |

## Sensitivity

The IR contains full prompts and tool input/output **verbatim** — the same
content the HTML report exposes, but in a form that flows easily into other
systems. v1 performs no redaction or PII scrubbing. Treat IR directories as
sensitive and scope their distribution accordingly.

## Versioning policy summary

- `dressage.ir/1.x` is additive and backward compatible within the major.
- Consumers: pin on `MAJOR`, ignore unknown fields, treat absent optional fields
  as zero values, and treat `blocks[].type` as an open set.
