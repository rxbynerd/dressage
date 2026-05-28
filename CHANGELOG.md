# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Provider abstraction so log sources other than AWS Bedrock can be added
  without forking the codebase. Every fetcher now emits a provider-neutral
  `model.Record` (with `Identity`/`Body`) and implements the new
  `fetch.Fetcher` interface; conversation reconstruction dispatches on a
  provider envelope family. This is the groundwork for upcoming providers
  (Azure OpenAI, Vertex AI / Gemini).
- Per-provider documentation under `docs/providers/` (Bedrock guide, plus
  planned-status stubs for Azure and Vertex) and a "Supported providers"
  table in the README.

### Changed

- **BREAKING (CLI):** Bedrock analysis now lives under a `bedrock` subcommand.
  The flat `dressage --bucket ...` invocation is replaced by
  `dressage bedrock --bucket ...`. The `--bucket`, `--prefix`, `--region`,
  and `--profile` flags are local to the `bedrock` subcommand.
- **BREAKING (CLI):** `--start`, `--end`, and `--output` are now persistent
  root flags shared across providers; they may be given before or after the
  subcommand. Running `dressage` with no subcommand prints help.
