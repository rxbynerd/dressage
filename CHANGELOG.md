# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

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

- **BREAKING (CLI):** Bedrock analysis now lives under a `bedrock` subcommand.
  The flat `dressage --bucket ...` invocation is replaced by
  `dressage bedrock --bucket ...`. The `--bucket`, `--prefix`, `--region`,
  and `--profile` flags are local to the `bedrock` subcommand.
- **BREAKING (CLI):** `--start`, `--end`, and `--output` are now persistent
  root flags shared across providers; they may be given before or after the
  subcommand. Running `dressage` with no subcommand prints help.
