package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spf13/cobra"

	"github.com/rxbynerd/dressage/internal/azurefetch"
	"github.com/rxbynerd/dressage/internal/fetch"
	"github.com/rxbynerd/dressage/internal/report"
	"github.com/rxbynerd/dressage/internal/s3fetch"
	"github.com/rxbynerd/dressage/internal/summary"
)

const dateFormat = "2006-01-02"

// version is set at build time via -ldflags "-X main.version=v1.2.3".
var version = "dev"

// commonFlags holds the provider-neutral flags shared by every subcommand.
type commonFlags struct {
	start  string
	end    string
	output string
}

func main() {
	var common commonFlags
	root := newRootCommand(&common)
	root.AddCommand(newBedrockCommand(&common))
	root.AddCommand(newAzureCommand(&common))
	root.AddCommand(newAzureStorageCommand(&common))

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

// newRootCommand builds the provider-neutral root command. With no subcommand
// it prints help. Shared flags (--start/--end/--output) are persistent so they
// apply to every provider subcommand.
func newRootCommand(common *commonFlags) *cobra.Command {
	root := &cobra.Command{
		Use:     "dressage",
		Short:   "Analyze hosted LLM model invocation logs",
		Version: version,
		Long: `Dressage fetches hosted LLM model invocation logs from a provider,
groups them into conversations, and generates a self-contained HTML report
for analyzing coding harness behaviour over the wire.

Choose a provider subcommand (e.g. "dressage bedrock") to ingest logs.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}

	pf := root.PersistentFlags()
	pf.StringVar(&common.start, "start", "", "Start date filter (YYYY-MM-DD)")
	pf.StringVar(&common.end, "end", "", "End date filter (YYYY-MM-DD)")
	pf.StringVar(&common.output, "output", "report.html", "Output HTML file path")

	return root
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

			return runReport(cmd.Context(), fetcher, "Bedrock Invocation Log Report", common)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&bucket, "bucket", "", "S3 bucket containing Bedrock invocation logs (required)")
	flags.StringVar(&prefix, "prefix", "", "S3 key prefix")
	flags.StringVar(&region, "region", "", "AWS region (default: from environment/config)")
	flags.StringVar(&profile, "profile", "", "AWS named profile")

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

			return runReport(cmd.Context(), fetcher, "Azure OpenAI Invocation Log Report", common)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&workspace, "workspace", "", "Log Analytics workspace ID (GUID) (required)")
	flags.StringVar(&subscription, "subscription", "", "Subscription ID narrowing filter")
	flags.StringVar(&resource, "resource", "", "Azure OpenAI resource ID (or substring) narrowing filter")
	flags.StringVar(&tenant, "tenant", "", "Microsoft Entra tenant ID for authentication")

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

			return runReport(cmd.Context(), fetcher, "Azure OpenAI Invocation Log Report", common)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&account, "account", "", "Azure Storage account name holding the diagnostic logs (required)")
	flags.StringVar(&container, "container", azurefetch.DefaultContainer, "Blob container holding the diagnostic logs")
	flags.StringVar(&tenant, "tenant", "", "Microsoft Entra tenant ID for authentication")

	_ = cmd.MarkFlagRequired("account")

	return cmd
}

// runReport is the shared pipeline tail: it parses the common date filters,
// fetches normalized records via the provider Fetcher, summarizes them, renders
// the report, and prints a summary block to stdout.
func runReport(ctx context.Context, fetcher fetch.Fetcher, title string, common *commonFlags) error {
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

	// Summarize.
	log.Println("Building summary...")
	rpt := summary.Summarize(records)
	rpt.Title = title

	// Generate report.
	log.Printf("Generating report to %s...", common.output)
	if err := report.Generate(rpt, common.output); err != nil {
		return fmt.Errorf("generating report: %w", err)
	}

	// Print summary to stdout.
	fmt.Println()
	fmt.Printf("Report written to %s\n", common.output)
	fmt.Printf("  Date range:   %s to %s\n", rpt.DateRange.Start.Format(dateFormat), rpt.DateRange.End.Format(dateFormat))
	fmt.Printf("  Invocations:  %d\n", rpt.TotalStats.InvocationCount)
	fmt.Printf("  Input tokens: %d\n", rpt.TotalStats.InputTokens)
	fmt.Printf("  Output tokens: %d\n", rpt.TotalStats.OutputTokens)
	fmt.Printf("  Errors:       %d\n", rpt.TotalStats.ErrorCount)
	fmt.Printf("  Days:         %d\n", len(rpt.Days))
	fmt.Printf("  Conversations: ")
	total := 0
	for _, d := range rpt.Days {
		total += len(d.Conversations)
	}
	fmt.Printf("%d\n", total)

	return nil
}
