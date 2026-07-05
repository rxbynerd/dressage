# Claude API (raw bodies)

The `claude` subcommand reconstructs conversations from raw Anthropic Messages
API request and response bodies captured on the local filesystem. Unlike the
hosted-provider subcommands it needs no cloud credentials or network access — it
only reads local files.

This is the right provider when you have Claude Code's raw-body capture enabled
(bodies written under `~/.claude/raw-api-bodies`) and want to analyze how the
harness talks to the Anthropic API directly, rather than through a hosted
gateway such as Bedrock or Vertex.

## Prerequisites

- A directory of captured bodies. By default Dressage reads
  `~/.claude/raw-api-bodies`; override with `--dir`.
- Nothing else — no credentials, no cloud access.

## Capture layout

The capture directory is flat and holds two disjoint sets of files, one JSON
body per file:

| File | Contents |
|------|----------|
| `<uuid>.request.json` | A Messages API **request** body: `model`, `messages[]`, `system`, `tools`, `metadata.user_id`, `diagnostics.previous_message_id`, ... |
| `req_<id>.response.json` | A Messages API **response** body: `id` (`msg_...`), `role`, `content[]`, `stop_reason`, `usage`. |

Claude Code resends the entire running transcript on every turn, so the latest
request of a session already contains the full conversation history; Dressage
reconstructs the conversation from it and appends the final assistant reply from
the matching response.

## How requests are paired with responses

Request and response filenames share no key — requests are named by an opaque
UUID and responses by the API request id. The bodies are correlated instead
through the **message-id chain**: each request's
`diagnostics.previous_message_id` holds the `id` of the *previous* turn's
response. Ordering a session's requests by growing message count, the response
produced by turn *i* is the body named by turn *i+1*'s `previous_message_id`.
This resolves for very nearly every captured turn.

The **final turn** of a session has no following request pointing back at its
response, so it cannot be located by id. Those terminal responses are matched by
write time instead: the earliest not-yet-claimed response of the same model
written at or just after the request. This heuristic only ever affects the
single last assistant turn of a session; if no response is found within a short
window the turn is left unpaired (its content and token counts are omitted).

## Timestamps

Neither the request nor the response body carries a wall-clock timestamp, so the
request file's **modification time** is used as the invocation time. The
`--start`/`--end` date filter is applied against this mtime.

## Session grouping and identity

Sessions are grouped by the `session_id` embedded in `metadata.user_id`, which on
this path is a JSON object, e.g.:

```json
{"device_id": "…", "account_uuid": "…", "session_id": "8d924171-…"}
```

The `account_uuid` becomes the record's principal; `session_id` and `device_id`
are recorded as identity attributes.

## Token accounting

Token counts come from the paired response's `usage` block: `input_tokens`,
`output_tokens`, `cache_read_input_tokens`, and `cache_creation_input_tokens`.
Requests with no paired response (see above) contribute no tokens. Latency and
first-byte timings are not present in the captured bodies and are not reported.

## Scale and memory

This capture can be very large — tens of thousands of files totaling many
gigabytes — and because Claude Code resends the whole transcript each turn, a
single busy day can hold gigabytes of request bodies in memory at once. Dressage
loads matching bodies fully into memory (no streaming), so:

- **Always scope with `--start`/`--end`.** Analyzing the entire directory at
  once is slow and memory-hungry. A single day is a good working unit.
- The self-contained HTML report embeds each invocation's raw body in a "Raw
  Invocations" drill-down. These are capped in size (with a truncation marker) so
  the report stays shareable; the full conversation is always available in the
  reconstructed conversation view above it.

## Usage

```bash
# Analyze a single day from the default capture directory
dressage claude --start 2026-07-04 --end 2026-07-04

# Point at a custom capture directory and write to a named file
dressage claude --dir /path/to/raw-api-bodies \
  --start 2026-07-01 --end 2026-07-04 --output week.html
```

### Flags

`--start`, `--end`, and `--output` are shared root flags (see the main README).
The `claude` subcommand adds:

| Flag | Default | Description |
|------|---------|-------------|
| `--dir` | `~/.claude/raw-api-bodies` | Directory of captured request/response bodies |

## Troubleshooting

- **"no request bodies … for the given window"** — the directory has no
  `*.request.json` files whose modification time falls in the date window. Widen
  or drop `--start`/`--end`, or check `--dir`.
- **The last assistant reply of a session is missing** — its terminal response
  fell outside the write-time match window (e.g. the session was still open when
  captured, or the response file was pruned). Earlier turns are unaffected.
- **The report is large or slow to open** — narrow the date window; a single day
  is the recommended working unit for this provider.
