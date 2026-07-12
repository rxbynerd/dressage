# DuckDB cookbook for the dressage IR

The IR's `facts.parquet` (one row per invocation) and `turns.parquet` (one row
per deduplicated turn, sidechains included) are designed to be queried
out-of-core by [DuckDB](https://duckdb.org) — no import step, no server. Every
query below was validated against a real Claude Code capture. Column
reference: [ir-format.md](ir-format.md).

Substitute your IR directory for `report.ir/` throughout. All examples run in
the `duckdb` CLI.

## Facts: spend, cache, errors, session shape

```sql
-- Token spend and cache-hit ratio by model
SELECT model_id, count(*) AS invocations,
       sum(input_tokens) AS input_tok, sum(output_tokens) AS output_tok,
       round(sum(cache_read_tokens) / nullif(sum(cache_read_tokens + input_tokens), 0), 3) AS cache_hit_ratio
FROM 'report.ir/facts.parquet'
GROUP BY 1 ORDER BY 2 DESC;

-- Spend per day
SELECT timestamp::date AS day, sum(input_tokens + cache_write_tokens) AS uncached_input,
       sum(cache_read_tokens) AS cached_input, sum(output_tokens) AS output
FROM 'report.ir/facts.parquet'
GROUP BY 1 ORDER BY 1;

-- Error rate by model
SELECT model_id, count(*) FILTER (error_code <> '') AS errors, count(*) AS total,
       round(count(*) FILTER (error_code <> '') / count(*)::double, 4) AS error_rate
FROM 'report.ir/facts.parquet'
GROUP BY 1 ORDER BY 4 DESC;

-- Session shape: invocations, threads (main + subagent sidechains), deepest
-- transcript per conversation
SELECT conversation_id, count(*) AS invocations,
       count(DISTINCT thread_id) AS threads, max(num_messages) AS deepest_transcript
FROM 'report.ir/facts.parquet'
GROUP BY 1 ORDER BY 2 DESC LIMIT 20;

-- Longest sessions by wall-clock duration
SELECT conversation_id, min(timestamp) AS started,
       max(timestamp) - min(timestamp) AS duration, count(*) AS invocations
FROM 'report.ir/facts.parquet'
GROUP BY 1 ORDER BY 3 DESC LIMIT 10;

-- Stop-reason distribution (populated for the claude provider)
SELECT stop_reason, count(*) FROM 'report.ir/facts.parquet'
WHERE stop_reason <> '' GROUP BY 1 ORDER BY 2 DESC;
```

Note: absent values are `''` / `0`, not NULL — filter with `<> ''`, not
`IS NOT NULL`.

## Turns: content analytics

```sql
-- Tool-use frequency, from the typed blocks JSON column
SELECT b->>'tool_name' AS tool, count(*) AS uses
FROM (SELECT unnest(from_json(blocks, '["json"]')) AS b
      FROM 'report.ir/turns.parquet')
WHERE b->>'type' = 'tool_use'
GROUP BY 1 ORDER BY 2 DESC LIMIT 20;

-- Main-thread vs sidechain (subagent) volume
SELECT CASE WHEN thread_id = '' THEN 'main' ELSE 'sidechain' END AS thread,
       count(*) AS turns
FROM 'report.ir/turns.parquet' GROUP BY 1;

-- Per-turn output-token distribution for assistant turns
SELECT round(quantile_cont(output_tokens, [0.5, 0.9, 0.99])::json) AS p50_p90_p99
FROM 'report.ir/turns.parquet'
WHERE role = 'assistant' AND has_metrics;
```

## Full-text search

DuckDB's FTS index lives in a `.duckdb` database, not in Parquet — so
materialize the turns once into a local database file and index it. The
database is a **derived, rebuildable cache**; the IR directory remains the
durable artifact.

```sql
-- one-time (or whenever the IR is regenerated):
--   duckdb sessions.duckdb
INSTALL fts; LOAD fts;
CREATE OR REPLACE TABLE turns AS
  SELECT row_number() OVER () AS rowid, *
  FROM 'report.ir/turns.parquet';
PRAGMA create_fts_index('turns', 'rowid', 'text', overwrite=1);

-- search (BM25 over the flattened turn text):
SELECT conversation_id, thread_id, turn_index, role,
       round(score, 2) AS score, substr(text, 1, 80) AS snippet
FROM (SELECT *, fts_main_turns.match_bm25(rowid, 'your search terms') AS score
      FROM turns)
WHERE score IS NOT NULL ORDER BY score DESC LIMIT 10;
```

## Combining runs

Each dressage run is one IR directory. DuckDB globs across them, and the
stable conversation ids dedup re-exported windows:

```sql
SELECT * FROM 'exports/*/facts.parquet';                -- union all runs
SELECT DISTINCT ON (conversation_id, request_uuid) *    -- dedup overlaps
FROM 'exports/*/facts.parquet';
```

## Wiring a review UI

A session-review web interface is a downstream program (dressage stays
ingest → normalize → export). Two proven shapes:

- **Local server:** a Go binary embedding DuckDB via the official
  [`github.com/duckdb/duckdb-go`](https://github.com/duckdb/duckdb-go) driver
  (CGO with prebuilt static libraries). On startup it materializes the
  `.duckdb` cache + FTS index from the IR (seconds), serves list/filter pages
  from `facts.parquet`, renders a conversation from `turns.parquet` (or the
  per-conversation JSON), and answers search from the FTS index.
- **Serverless:** [duckdb-wasm](https://github.com/duckdb/duckdb-wasm) in the
  browser queries the Parquet files directly over HTTP range requests from
  static hosting. Note some hosts (GitHub Pages, Cloudflare Pages) don't
  support range requests and force full-file reads; the FTS extension is not a
  given in wasm — prefer the local-server shape when search matters.
