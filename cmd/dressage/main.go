package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spf13/cobra"

	"github.com/rxbynerd/dressage/internal/azurefetch"
	"github.com/rxbynerd/dressage/internal/fetch"
	"github.com/rxbynerd/dressage/internal/ir"
	"github.com/rxbynerd/dressage/internal/rawfetch"
	"github.com/rxbynerd/dressage/internal/s3fetch"
	"github.com/rxbynerd/dressage/internal/serve"
	"github.com/rxbynerd/dressage/internal/summary"
	"github.com/rxbynerd/dressage/internal/vertexfetch"
)

const dateFormat = "2006-01-02"

// version is set at build time via -ldflags "-X main.version=v1.2.3".
var version = "dev"

// commonFlags holds the ingestion flags shared by every provider subcommand.
type commonFlags struct {
	start     string
	end       string
	out       string // IR output directory
	rawBodies string // raw bodies in the IR: "omit" (default) or "embed"
}

func main() {
	// Stamp the IR exporter's tool version from the build-time version so the
	// manifest records the producing tool accurately.
	ir.Version = version

	var common commonFlags
	root := newRootCommand()
	root.AddCommand(newBedrockCommand(&common))
	root.AddCommand(newAzureCommand(&common))
	root.AddCommand(newAzureStorageCommand(&common))
	root.AddCommand(newVertexCommand(&common))
	root.AddCommand(newClaudeCommand(&common))
	root.AddCommand(newServeCommand())

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

// newRootCommand builds the provider-neutral root command. With no subcommand
// it prints help.
func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:     "dressage",
		Short:   "Analyze hosted LLM model invocation logs",
		Version: version,
		Long: `Dressage fetches hosted LLM model invocation logs from a provider,
groups them into conversations, and exports a versioned, provider-neutral IR
directory for analyzing coding harness behaviour over the wire.

Choose a provider subcommand (e.g. "dressage bedrock") to ingest logs into an
IR directory, then "dressage serve <ir-dir>" to browse it locally.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	return root
}

// addIngestFlags registers the ingestion flags shared by every provider
// subcommand. They live on the provider subcommands (not the root) so the
// serve command — which ingests nothing — does not inherit them.
func addIngestFlags(cmd *cobra.Command, common *commonFlags) {
	flags := cmd.Flags()
	flags.StringVar(&common.start, "start", "", "Start date filter (YYYY-MM-DD)")
	flags.StringVar(&common.end, "end", "", "End date filter (YYYY-MM-DD)")
	flags.StringVar(&common.out, "out", "report.ir", "IR output directory")
	flags.StringVar(&common.rawBodies, "raw-bodies", "omit", "Raw request/response bodies in the IR: omit or embed (embedding is quadratic for providers that resend the transcript every turn)")
}

// newBedrockCommand builds the "bedrock" subcommand, which ingests AWS Bedrock
// model invocation logs from S3 and renders them via the shared report pipeline.
func newBedrockCommand(common *commonFlags) *cobra.Command {
	var (
		bucket  string
		prefix  string
		region  string
		profile string
	)

	cmd := &cobra.Command{
		Use:          "bedrock",
		Short:        "Analyze AWS Bedrock invocation logs from S3",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Configure AWS SDK.
			var cfgOpts []func(*config.LoadOptions) error
			if region != "" {
				cfgOpts = append(cfgOpts, config.WithRegion(region))
			}
			if profile != "" {
				cfgOpts = append(cfgOpts, config.WithSharedConfigProfile(profile))
			}

			cfg, err := config.LoadDefaultConfig(cmd.Context(), cfgOpts...)
			if err != nil {
				return fmt.Errorf("loading AWS config: %w", err)
			}

			s3Client := s3.NewFromConfig(cfg)
			log.Println("Fetching Bedrock invocation logs from S3...")
			fetcher := s3fetch.New(s3Client, bucket, prefix)

			return runReport(cmd.Context(), fetcher, "bedrock", common)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&bucket, "bucket", "", "S3 bucket containing Bedrock invocation logs (required)")
	flags.StringVar(&prefix, "prefix", "", "S3 key prefix")
	flags.StringVar(&region, "region", "", "AWS region (default: from environment/config)")
	flags.StringVar(&profile, "profile", "", "AWS named profile")
	addIngestFlags(cmd, common)

	_ = cmd.MarkFlagRequired("bucket")

	return cmd
}

// newAzureCommand builds the "azure" subcommand, which ingests Azure OpenAI
// RequestResponse invocation logs from an Azure Monitor Log Analytics workspace
// and renders them via the shared report pipeline.
func newAzureCommand(common *commonFlags) *cobra.Command {
	var (
		workspace    string
		subscription string
		resource     string
		tenant       string
	)

	cmd := &cobra.Command{
		Use:          "azure",
		Short:        "Analyze Azure OpenAI invocation logs from Log Analytics",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			var credOpts *azidentity.DefaultAzureCredentialOptions
			if tenant != "" {
				credOpts = &azidentity.DefaultAzureCredentialOptions{TenantID: tenant}
			}

			cred, err := azidentity.NewDefaultAzureCredential(credOpts)
			if err != nil {
				return fmt.Errorf("creating Azure credential: %w", err)
			}

			log.Println("Querying Azure OpenAI invocation logs from Log Analytics...")
			fetcher, err := azurefetch.New(cred, workspace, subscription, resource)
			if err != nil {
				return fmt.Errorf("creating Azure fetcher: %w", err)
			}

			return runReport(cmd.Context(), fetcher, "azure", common)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&workspace, "workspace", "", "Log Analytics workspace ID (GUID) (required)")
	flags.StringVar(&subscription, "subscription", "", "Subscription ID narrowing filter")
	flags.StringVar(&resource, "resource", "", "Azure OpenAI resource ID (or substring) narrowing filter")
	flags.StringVar(&tenant, "tenant", "", "Microsoft Entra tenant ID for authentication")
	addIngestFlags(cmd, common)

	_ = cmd.MarkFlagRequired("workspace")

	return cmd
}

// newAzureStorageCommand builds the "azure-storage" subcommand, which ingests
// Azure OpenAI RequestResponse diagnostic logs that have been exported to an
// Azure Storage account (rather than a Log Analytics workspace) and renders them
// via the shared report pipeline. The logs normalize identically to the "azure"
// subcommand; only the sink differs.
func newAzureStorageCommand(common *commonFlags) *cobra.Command {
	var (
		account   string
		container string
		tenant    string
	)

	cmd := &cobra.Command{
		Use:          "azure-storage",
		Short:        "Analyze Azure OpenAI invocation logs from a Storage account",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			var credOpts *azidentity.DefaultAzureCredentialOptions
			if tenant != "" {
				credOpts = &azidentity.DefaultAzureCredentialOptions{TenantID: tenant}
			}

			cred, err := azidentity.NewDefaultAzureCredential(credOpts)
			if err != nil {
				return fmt.Errorf("creating Azure credential: %w", err)
			}

			log.Println("Reading Azure OpenAI diagnostic logs from Storage account...")
			fetcher, err := azurefetch.NewBlobFetcher(cred, account, container)
			if err != nil {
				return fmt.Errorf("creating Azure storage fetcher: %w", err)
			}

			return runReport(cmd.Context(), fetcher, "azure", common)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&account, "account", "", "Azure Storage account name holding the diagnostic logs (required)")
	flags.StringVar(&container, "container", azurefetch.DefaultContainer, "Blob container holding the diagnostic logs")
	flags.StringVar(&tenant, "tenant", "", "Microsoft Entra tenant ID for authentication")
	addIngestFlags(cmd, common)

	_ = cmd.MarkFlagRequired("account")

	return cmd
}

// newVertexCommand builds the "vertex" subcommand, which ingests Google Vertex
// AI request-response invocation logs from a BigQuery dataset and renders them
// via the shared report pipeline. Gemini invocations are reconstructed into full
// conversations; non-Gemini (e.g. Claude-on-Vertex) invocations contribute to
// summary stats but are not yet reconstructed (tracked in issue #4).
func newVertexCommand(common *commonFlags) *cobra.Command {
	var (
		project     string
		dataset     string
		table       string
		location    string
		credentials string
	)

	cmd := &cobra.Command{
		Use:          "vertex",
		Short:        "Analyze Google Vertex AI / Gemini invocation logs from BigQuery",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := vertexfetch.NewClient(cmd.Context(), project, credentials)
			if err != nil {
				return fmt.Errorf("creating Vertex BigQuery client: %w", err)
			}
			defer client.Close()

			log.Println("Querying Vertex AI invocation logs from BigQuery...")
			fetcher := vertexfetch.New(client, project, dataset, table, location)

			return runReport(cmd.Context(), fetcher, "vertex", common)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&project, "project", "", "GCP project containing the BigQuery logging dataset (required)")
	flags.StringVar(&dataset, "dataset", "", "BigQuery dataset holding the request-response logs (required)")
	flags.StringVar(&table, "table", "", "BigQuery table holding the request-response logs (required)")
	flags.StringVar(&location, "location", "", "BigQuery dataset location (e.g. us-central1; inferred if empty)")
	flags.StringVar(&credentials, "credentials", "", "Path to a service-account key JSON file (default: Application Default Credentials)")
	addIngestFlags(cmd, common)

	_ = cmd.MarkFlagRequired("project")
	_ = cmd.MarkFlagRequired("dataset")
	_ = cmd.MarkFlagRequired("table")

	return cmd
}

// newClaudeCommand builds the "claude" subcommand, which reconstructs
// conversations from raw Anthropic Messages API request/response bodies captured
// on the local filesystem (by default under ~/.claude/raw-api-bodies, as written
// by Claude Code when raw-body capture is enabled). Unlike the hosted-provider
// subcommands it needs no cloud credentials; it only reads local files.
func newClaudeCommand(common *commonFlags) *cobra.Command {
	var dir string

	cmd := &cobra.Command{
		Use:          "claude",
		Short:        "Analyze raw Claude API request/response bodies from a local directory",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if dir == "" {
				dir = defaultRawBodiesDir()
			}
			log.Printf("Reading raw Claude API bodies from %s...", dir)
			fetcher := rawfetch.New(dir)

			return runReport(cmd.Context(), fetcher, "claude", common)
		},
	}

	cmd.Flags().StringVar(&dir, "dir", "", "Directory of captured request/response bodies (default: ~/.claude/raw-api-bodies)")
	addIngestFlags(cmd, common)

	return cmd
}

// defaultRawBodiesDir returns the default capture directory,
// ~/.claude/raw-api-bodies, falling back to a relative path if the home
// directory cannot be resolved.
func defaultRawBodiesDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".claude", "raw-api-bodies")
	}
	return filepath.Join(home, ".claude", "raw-api-bodies")
}

// runReport is the shared ingestion pipeline tail: it parses the common date
// filters, fetches normalized records via the provider Fetcher, groups them,
// and streams the machine-readable IR to disk one conversation at a time (so
// peak memory stays bounded to the largest single transcript), then prints a
// summary block to stdout. provider identifies the active subcommand, recorded
// in the IR manifest's source metadata.
func runReport(ctx context.Context, fetcher fetch.Fetcher, provider string, common *commonFlags) error {
	var embedRawBodies bool
	switch common.rawBodies {
	case "", "omit": // "" = flag default when commonFlags is constructed directly
	case "embed":
		embedRawBodies = true
	default:
		return fmt.Errorf("invalid --raw-bodies %q: expected omit or embed", common.rawBodies)
	}
	irDir := common.out
	if irDir == "" {
		irDir = "report.ir"
	}

	// Parse date filters.
	var startDate, endDate time.Time
	var err error
	if common.start != "" {
		startDate, err = time.Parse(dateFormat, common.start)
		if err != nil {
			return fmt.Errorf("invalid --start date %q: expected YYYY-MM-DD", common.start)
		}
	}
	if common.end != "" {
		endDate, err = time.Parse(dateFormat, common.end)
		if err != nil {
			return fmt.Errorf("invalid --end date %q: expected YYYY-MM-DD", common.end)
		}
		// Make end date inclusive by advancing to the next day.
		endDate = endDate.AddDate(0, 0, 1)
	}

	// Fetch records.
	records, err := fetcher.Fetch(ctx, startDate, endDate)
	if err != nil {
		return fmt.Errorf("fetching logs: %w", err)
	}

	if len(records) == 0 {
		log.Println("No invocation logs found for the specified criteria.")
	}

	// Group records into days and conversations without materializing any
	// conversation content yet.
	log.Println("Building summary...")
	plan := summary.NewPlan(records)
	rpt := plan.Report()

	// Open the streaming IR exporter before materialization so conversations
	// flow straight to disk.
	src := ir.SourceInfo{
		Provider: provider,
		Command:  commandString(),
		DateRange: ir.ManifestDateRng{
			Start: common.start,
			End:   common.end,
		},
	}
	log.Printf("Exporting IR to %s...", irDir)
	exporter, err := ir.NewExporter(irDir, src, rpt.GeneratedAt, ir.ExportOptions{RawBodies: embedRawBodies})
	if err != nil {
		return fmt.Errorf("exporting IR: %w", err)
	}

	// Materialize conversations one at a time, streaming each to the exporter.
	// Nothing is retained, so peak memory stays bounded to a single transcript.
	convCount := 0
	for cs := range plan.Conversations(summary.MaterializeOptions{}) {
		convCount++
		if err := exporter.WriteConversation(*cs); err != nil {
			return fmt.Errorf("exporting IR: %w", err)
		}
	}
	if err := exporter.Finish(rpt); err != nil {
		return fmt.Errorf("exporting IR: %w", err)
	}

	// Print summary to stdout.
	fmt.Println()
	fmt.Printf("IR written to %s (%d conversation file(s))\n", irDir, convCount)
	fmt.Printf("  View it with: dressage serve %s\n", irDir)
	fmt.Printf("  Date range:   %s to %s\n", rpt.DateRange.Start.Format(dateFormat), rpt.DateRange.End.Format(dateFormat))
	fmt.Printf("  Invocations:  %d\n", rpt.TotalStats.InvocationCount)
	fmt.Printf("  Input tokens: %d\n", rpt.TotalStats.InputTokens)
	fmt.Printf("  Output tokens: %d\n", rpt.TotalStats.OutputTokens)
	fmt.Printf("  Errors:       %d\n", rpt.TotalStats.ErrorCount)
	fmt.Printf("  Days:         %d\n", len(rpt.Days))
	fmt.Printf("  Conversations: %d\n", convCount)

	return nil
}

// newServeCommand builds the "serve" subcommand: it opens an existing IR
// directory and serves a browsable, localhost-only web UI over it — the
// replacement for the retired static HTML report. It ingests nothing.
func newServeCommand() *cobra.Command {
	var addr string

	cmd := &cobra.Command{
		Use:          "serve <ir-dir>",
		Short:        "Serve a browsable web UI over an IR directory on localhost",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			reader, err := ir.OpenDir(args[0])
			if err != nil {
				return fmt.Errorf("opening IR directory: %w", err)
			}
			srv := serve.New(reader)
			m := reader.Manifest()
			log.Printf("Serving %d conversation(s) from %s", len(m.Conversations), args[0])
			fmt.Printf("dressage serve: http://%s\n", browserAddr(addr))
			if err := srv.ListenAndServe(addr); err != nil {
				return fmt.Errorf("serving: %w", err)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:7878", "Address to bind the local web server to")

	return cmd
}

// browserAddr turns a bind address into one suitable for a browser URL: a bare
// ":port" (bind on all interfaces) is shown as localhost:port.
func browserAddr(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "localhost" + addr
	}
	return addr
}

// sensitiveFlags are flags whose values may identify or grant access to a
// resource (credential paths, account/subscription ids, workspace/tenant
// GUIDs). Their values are redacted from the command string recorded in the IR
// manifest, which is the artifact most likely to be shared, archived, or fed to
// an analysis program.
var sensitiveFlags = map[string]bool{
	"--credentials":  true,
	"--profile":      true,
	"--subscription": true,
	"--workspace":    true,
	"--tenant":       true,
	"--account":      true,
}

// commandString reconstructs the invoked command line for the IR manifest's
// source metadata, so a downstream consumer can see how the IR was produced,
// with the values of sensitive flags redacted. It handles both the
// "--flag value" and "--flag=value" spellings.
func commandString() string {
	out := make([]string, 0, len(os.Args))
	redactNext := false
	for _, a := range os.Args {
		if redactNext {
			out = append(out, "<redacted>")
			redactNext = false
			continue
		}
		// "--flag=value": redact the value, keep the flag name.
		if eq := strings.IndexByte(a, '='); eq > 0 && sensitiveFlags[a[:eq]] {
			out = append(out, a[:eq]+"=<redacted>")
			continue
		}
		// "--flag value": redact the following arg.
		if sensitiveFlags[a] {
			redactNext = true
		}
		out = append(out, a)
	}
	return strings.Join(out, " ")
}
