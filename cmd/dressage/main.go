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

	"github.com/rubynerd/dressage/internal/report"
	"github.com/rubynerd/dressage/internal/s3fetch"
	"github.com/rubynerd/dressage/internal/summary"
)

const dateFormat = "2006-01-02"

func main() {
	var (
		bucket  string
		prefix  string
		region  string
		profile string
		start   string
		end     string
		output  string
	)

	cmd := &cobra.Command{
		Use:   "dressage",
		Short: "Analyze AWS Bedrock model invocation logs",
		Long: `Dressage fetches Bedrock model invocation logs from an S3 bucket,
groups them into conversations, and generates a self-contained HTML report
for analyzing coding harness behaviour over the wire.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cmd.Context(), bucket, prefix, region, profile, start, end, output)
		},
		SilenceUsage: true,
	}

	flags := cmd.Flags()
	flags.StringVar(&bucket, "bucket", "", "S3 bucket containing Bedrock invocation logs (required)")
	flags.StringVar(&prefix, "prefix", "", "S3 key prefix")
	flags.StringVar(&region, "region", "", "AWS region (default: from environment/config)")
	flags.StringVar(&profile, "profile", "", "AWS named profile")
	flags.StringVar(&start, "start", "", "Start date filter (YYYY-MM-DD)")
	flags.StringVar(&end, "end", "", "End date filter (YYYY-MM-DD)")
	flags.StringVar(&output, "output", "report.html", "Output HTML file path")

	_ = cmd.MarkFlagRequired("bucket")

	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func run(ctx context.Context, bucket, prefix, region, profile, startStr, endStr, output string) error {
	// Parse date filters.
	var startDate, endDate time.Time
	var err error
	if startStr != "" {
		startDate, err = time.Parse(dateFormat, startStr)
		if err != nil {
			return fmt.Errorf("invalid --start date %q: expected YYYY-MM-DD", startStr)
		}
	}
	if endStr != "" {
		endDate, err = time.Parse(dateFormat, endStr)
		if err != nil {
			return fmt.Errorf("invalid --end date %q: expected YYYY-MM-DD", endStr)
		}
		// Make end date inclusive by advancing to the next day.
		endDate = endDate.AddDate(0, 0, 1)
	}

	// Configure AWS SDK.
	var cfgOpts []func(*config.LoadOptions) error
	if region != "" {
		cfgOpts = append(cfgOpts, config.WithRegion(region))
	}
	if profile != "" {
		cfgOpts = append(cfgOpts, config.WithSharedConfigProfile(profile))
	}

	cfg, err := config.LoadDefaultConfig(ctx, cfgOpts...)
	if err != nil {
		return fmt.Errorf("loading AWS config: %w", err)
	}

	s3Client := s3.NewFromConfig(cfg)

	// Fetch logs.
	log.Println("Fetching Bedrock invocation logs from S3...")
	fetcher := s3fetch.New(s3Client, bucket, prefix)
	logs, err := fetcher.Fetch(ctx, startDate, endDate)
	if err != nil {
		return fmt.Errorf("fetching logs: %w", err)
	}

	if len(logs) == 0 {
		log.Println("No invocation logs found for the specified criteria.")
	}

	// Summarize.
	log.Println("Building summary...")
	rpt := summary.Summarize(logs)

	// Generate report.
	log.Printf("Generating report to %s...", output)
	if err := report.Generate(rpt, output); err != nil {
		return fmt.Errorf("generating report: %w", err)
	}

	// Print summary to stdout.
	fmt.Println()
	fmt.Printf("Report written to %s\n", output)
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
