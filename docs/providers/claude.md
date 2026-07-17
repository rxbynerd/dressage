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
response. Ordering a chain's requests by growing message count, the response
produced by turn *i* is the body named by turn *i+1*'s `previous_message_id`.
This resolves for very nearly every captured turn.

A session is not one linear chain: subagent **sidechains** run interleaved with
the main thread. Requests are therefore first split into chains by a hash of
their first message — a linear thread's first message is invariant as its
transcript grows (the opening user prompt for the main thread, the task prompt
for a subagent) — and pairing runs within each chain. Each chain becomes a
**thread**: the main thread (the chain that starts earliest) plus one sidechain
per subagent, and each record carries its thread id (the chain root's request
uuid). Compaction rewrites the transcript and so starts a new chain whose root
names the old context's final response; such chains are stitched back into one
thread, and that final response is paired to the old chain's tip even when the
session resumed much later.

Each chain's **tip** (its last request) has no successor to name its response,
so it cannot be located by id. Those terminal responses are matched by write
time instead: the earliest not-yet-claimed response of the same model written
at or just after the request. If no response is found within a short window the
turn is left unpaired (its content and token counts are omitted).

One caveat: two chains whose first messages are byte-identical (e.g. two
parallel subagents given the exact same prompt) share a chain key and merge;
pairing inside the merged group falls back to message-count adjacency. Real
prompts are effectively always distinct.

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
The response's `stop_reason` and message id are lifted into the record (they
appear in the IR facts table).

## Reconstruction and sidechains

The reconstructed conversation view is the **main thread**. Subagent sidechains
are reconstructed separately, one transcript per chain, and exported in the IR
both inline in each conversation file (`conversation.sidechains[]`) and in
`turns.parquet` with their thread id; `dressage serve` renders them on the
conversation page. On multi-agent workflows sidechains are often the majority of
the content — on a real capture, ~57% of all deduplicated turns.

## Scale and memory

This capture can be very large — a hundred thousand files totaling tens of
gigabytes — but Dressage does **not** hold bodies in memory: records carry
file-backed lazy references, grouping uses metadata only, and conversations
materialize one at a time during export. Body files are read at export time
(not snapshot at fetch), so a file deleted mid-run degrades that one body
rather than failing the run.

Measured against a real capture (M-series laptop, local SSD): a busy day of
7.6k invocations (~1.9 GB of bodies after windowing, drawn from the 25 GB
directory) exports the IR in ~5 s with ~200 MB peak memory; the **entire
25 GB / 112k-file capture** exports in one run with ~700 MB peak memory,
producing a 370 MB IR. Before the lazy-body pipeline a single smaller day
required ~15.7 GB.

- `--start`/`--end` bound run time (fewer files to read), but ingestion is
  memory-bounded regardless of window: conversations stream to the IR one at a
  time.
- The IR omits raw bodies by default (`--raw-bodies embed` restores them; see
  [docs/ir-format.md](../ir-format.md)) — with embedding on, a resend-style
  capture's IR grows quadratically with conversation length. `dressage serve`
  shows each embedded body as a size-capped preview with a link to the full
  JSON, so a large embedded IR still browses cheaply.

## Usage

```bash
# Analyze a single day from the default capture directory
dressage claude --start 2026-07-04 --end 2026-07-04

# Point at a custom capture directory and write to a named IR directory
dressage claude --dir /path/to/raw-api-bodies \
  --start 2026-07-01 --end 2026-07-04 --out week.ir
```

### Flags

`--start`, `--end`, `--out`, and `--raw-bodies` are shared ingestion flags (see
the main README). The `claude` subcommand adds:

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
- **Ingesting a very large window is slow** — narrow the date window; a single
  day is a good working unit for this provider. Browsing the resulting IR with
  `dressage serve` is memory-bounded regardless of capture size.
