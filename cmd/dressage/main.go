package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spf13/cobra"

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
