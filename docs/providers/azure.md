# Azure OpenAI

Analyze [Azure OpenAI request/response logs](https://learn.microsoft.com/en-us/azure/ai-foundry/openai/how-to/monitor-openai)
that Azure Monitor delivers, via a diagnostic setting, to a Log Analytics
workspace where Dressage queries them with KQL.

Dressage ingests from **Log Analytics** (KQL gives server-side date filtering,
which keeps memory bounded, and it is the destination Microsoft's own dashboards
use). Storage Account and Event Hub destinations are out of scope.

## Prerequisites

- Go 1.23+ (only to build from source).
- An Azure subscription with an Azure OpenAI resource.
- A Log Analytics workspace to receive the diagnostic logs.
- A diagnostic setting on the Azure OpenAI resource forwarding the
  `RequestResponse` category to that workspace (see below).
- Azure credentials resolvable by the Azure SDK (environment variables, a
  managed identity, a workload identity, or `az login`) with the
  [required RBAC](#required-rbac) on the workspace.

## Enabling Azure OpenAI request/response logging

Dressage reads logs that already exist in Log Analytics; it does not enable
logging for you. Configure a diagnostic setting on the Azure OpenAI resource
that forwards the `RequestResponse` category.

### Using the Azure CLI

```bash
az monitor diagnostic-settings create \
  --name aoai-to-law \
  --resource <Azure OpenAI resource ID> \
  --workspace <Log Analytics workspace resource ID> \
  --logs '[{"category":"RequestResponse","enabled":true}]'
```

> **Do not** pass `--export-to-resource-specific`. Azure OpenAI (Cognitive
> Services) has no resource-specific table — that flag is a no-op for it, and
> the rows land in the shared `AzureDiagnostics` table regardless. Dressage
> queries `AzureDiagnostics`.

### Using the Azure Portal

1. Open the **Azure OpenAI** resource in the Portal.
2. Under **Monitoring**, select **Diagnostic settings** → **Add diagnostic
   setting**.
3. Give the setting a name (e.g. `aoai-to-law`).
4. Under **Logs**, tick the **RequestResponse** category.
5. Under **Destination details**, tick **Send to Log Analytics workspace** and
   choose the subscription and workspace.
6. **Save.** The first records can take a few minutes to appear after the next
   request.

## Content-logging caveat

Whether the prompt and completion **bodies** appear in the logs depends on the
Azure OpenAI resource's abuse-monitoring configuration — this is a property of
the resource, not of Dressage, and Dressage cannot work around it.

Customers approved for **modified abuse monitoring** (the customer-managed-key
opt-out from content storage) may see `RequestResponse` rows with **no payload
content** at all. In that case Dressage still reports the invocation metadata
(timestamps, operation, status, latency, identity) but cannot reconstruct the
conversation, because there is no request/response body to parse.

You can detect whether content storage is off for a resource:

```bash
az cognitiveservices account show \
  --name <resource name> --resource-group <rg> \
  --query "properties.capabilities"
```

A `ContentLogging: false` capability appears when content storage is **off**.

References:

- [Monitor Azure OpenAI](https://learn.microsoft.com/en-us/azure/ai-foundry/openai/how-to/monitor-openai)
- [Azure OpenAI data privacy](https://learn.microsoft.com/en-us/azure/ai-foundry/responsible-ai/openai/data-privacy)
- [Abuse monitoring](https://learn.microsoft.com/en-us/azure/ai-foundry/openai/concepts/abuse-monitoring)

## How Dressage finds payloads

Microsoft does **not** document the exact field names used inside the
`RequestResponse` log's property bag for the request body, the response body,
the model/deployment name, or the Entra caller identity. They are not reliably
promoted to dedicated `*_s` columns; instead they live inside the dynamic
`properties_s` column as a stringified-JSON property bag (sometimes itself
containing further stringified JSON).

To cope, Dressage projects `properties_s`, parses it as JSON, then digs out each
field by trying a **list of candidate key names** (case-insensitively). If your
workspace's rows use different names, the candidate lists may need adjusting.
They are centralized at the top of
[`internal/azurefetch/azure.go`](../../internal/azurefetch/azure.go)
(`requestBodyKeys`, `responseBodyKeys`, `deploymentKeys`, `callerOIDKeys`) with
a comment marking them as unverified against a real workspace.

A field value may be double-encoded (a JSON string whose contents are
themselves JSON); Dressage unwraps that automatically.

## Required RBAC

The credential Dressage uses needs, at a minimum, **Log Analytics Reader** on
the Log Analytics workspace (this grants read access to query the workspace's
data). Assign it on the workspace (or an enclosing scope):

```bash
az role assignment create \
  --assignee <principal object ID or sign-in name> \
  --role "Log Analytics Reader" \
  --scope <Log Analytics workspace resource ID>
```

Authentication uses `DefaultAzureCredential`, which honours (in order)
environment variables, workload identity, managed identity, and `az login`.
Pass `--tenant` to pin the tenant when your `az login` spans multiple tenants.

## Finding the workspace ID

`--workspace` is the Log Analytics **workspace ID** (a GUID), not the ARM
resource ID. In the Portal: open the **Log Analytics workspace** → **Overview**
→ copy the **Workspace ID** field.

## Cost callouts

Sending `RequestResponse` logs to Log Analytics incurs **ingestion** and
**retention** charges billed per GB. Request/response bodies for long coding
sessions can be large, so monitor the workspace's data volume and set a
retention period appropriate to your needs. See
[Azure Monitor pricing](https://azure.microsoft.com/en-us/pricing/details/monitor/).

## Usage

`--start`, `--end`, and `--output` are persistent (root) flags and may appear
before or after the `azure` subcommand. The Azure-specific flags are local to
`azure`.

```bash
# Analyze all RequestResponse logs in a workspace
dressage azure --workspace 11111111-2222-3333-4444-555555555555

# Filter to a date range
dressage azure --workspace 11111111-2222-3333-4444-555555555555 \
  --start 2025-03-01 --end 2025-03-15

# Narrow to one resource and pin the tenant, writing to a named file
dressage azure --workspace 11111111-2222-3333-4444-555555555555 \
  --resource my-aoai --tenant <tenant-guid> --output march-report.html
```

### Flags

Persistent (root) flags, shared with every provider:

| Flag | Default | Description |
|------|---------|-------------|
| `--start` | | Start date filter (YYYY-MM-DD, inclusive) |
| `--end` | | End date filter (YYYY-MM-DD, inclusive) |
| `--output` | `report.html` | Output HTML file path |

`azure`-specific flags:

| Flag | Required | Default | Description |
|------|----------|---------|-------------|
| `--workspace` | Yes | | Log Analytics workspace ID (GUID) |
| `--subscription` | No | | Subscription ID narrowing filter |
| `--resource` | No | | Azure OpenAI resource ID (or substring) narrowing filter |
| `--tenant` | No | | Microsoft Entra tenant ID for authentication |

## Conversation grouping

As with Bedrock, Dressage performs rich conversation reconstruction (the
turn-by-turn drill-down) only for **session-grouped** conversations. A session
id is extracted from the request body's top-level `user` field — Claude Code
sets this to a value containing a `_session_` marker. Rows without a session id
fall back to the time-gap heuristic (same provider + deployment + caller
identity, within a 5-minute gap) and appear in the report without the
turn-by-turn Detail. This is intentional and consistent with the Bedrock
behaviour.

## Troubleshooting

- **No rows returned.** The diagnostic setting may not be enabled, content
  logging may be off (see [the caveat](#content-logging-caveat)), or you may be
  querying the wrong workspace. Confirm rows exist directly with a KQL query:
  `AzureDiagnostics | where Category == "RequestResponse" | take 10`.
- **401 / 403.** The credential lacks the **Log Analytics Reader** role on the
  workspace. Assign it (see [Required RBAC](#required-rbac)).
- **"Table not found" or empty results right after enabling.** The resource is
  not logging yet; the first request after enabling a diagnostic setting can
  take a few minutes to surface in Log Analytics.
- **Rows appear but conversations have no payloads.** Either content logging is
  off for the resource, or your workspace uses property-bag field names Dressage
  does not recognise — see [How Dressage finds payloads](#how-dressage-finds-payloads).
