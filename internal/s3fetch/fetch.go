// Package s3fetch retrieves and parses AWS Bedrock model invocation logs from S3.
package s3fetch

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/rubynerd/dressage/internal/model"
)

// s3API is the subset of the S3 client interface used by Fetcher,
// enabling substitution in tests.
type s3API interface {
	ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

// Fetcher downloads and parses Bedrock invocation logs from S3.
type Fetcher struct {
	client s3API
	bucket string
	prefix string
}

// New creates a Fetcher that reads logs from the given S3 bucket and prefix.
func New(client *s3.Client, bucket, prefix string) *Fetcher {
	return &Fetcher{
		client: client,
		bucket: bucket,
		prefix: strings.TrimRight(prefix, "/"),
	}
}

// Fetch lists all .json.gz log files under the configured prefix, optionally
// filtered to the [start, end) time range, then downloads, decompresses, and
// parses each file into InvocationLog records.
func (f *Fetcher) Fetch(ctx context.Context, start, end time.Time) ([]model.InvocationLog, error) {
	keys, err := f.listLogFiles(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing log files: %w", err)
	}

	// Filter keys by date range derived from the S3 key path structure:
	// {prefix}/AWSLogs/{account}/BedrockModelInvocationLogs/{region}/YYYY/MM/DD/HH/*.json.gz
	if !start.IsZero() || !end.IsZero() {
		keys = filterKeysByDate(keys, start, end)
	}

	log.Printf("Found %d log files", len(keys))

	var allLogs []model.InvocationLog
	for i, key := range keys {
		log.Printf("Processing file %d/%d: %s", i+1, len(keys), key)

		records, err := f.downloadAndParse(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("processing %s: %w", key, err)
		}
		allLogs = append(allLogs, records...)
	}

	log.Printf("Parsed %d records", len(allLogs))
	return allLogs, nil
}

// listLogFiles pages through S3 and returns all keys ending in .json.gz.
func (f *Fetcher) listLogFiles(ctx context.Context) ([]string, error) {
	var keys []string
	var continuationToken *string

	for {
		input := &s3.ListObjectsV2Input{
			Bucket:            aws.String(f.bucket),
			Prefix:            aws.String(f.prefix),
			ContinuationToken: continuationToken,
		}

		output, err := f.client.ListObjectsV2(ctx, input)
		if err != nil {
			return nil, err
		}

		for _, obj := range output.Contents {
			key := aws.ToString(obj.Key)
			if strings.HasSuffix(key, ".json.gz") {
				keys = append(keys, key)
			}
		}

		if !aws.ToBool(output.IsTruncated) {
			break
		}
		continuationToken = output.NextContinuationToken
	}

	return keys, nil
}

// filterKeysByDate keeps only keys whose YYYY/MM/DD path components fall
// within the [start, end) range. Keys that cannot be parsed are kept
// (conservative — don't silently drop data).
func filterKeysByDate(keys []string, start, end time.Time) []string {
	var filtered []string
	for _, key := range keys {
		t, ok := parseDateFromKey(key)
		if !ok {
			// Can't determine date; keep the key to avoid data loss.
			filtered = append(filtered, key)
			continue
		}
		if !start.IsZero() && t.Before(start) {
			continue
		}
		if !end.IsZero() && !t.Before(end) {
			continue
		}
		filtered = append(filtered, key)
	}
	return filtered
}

// parseDateFromKey extracts a date from the YYYY/MM/DD portion of a Bedrock
// log key path. Returns the parsed date and true on success.
func parseDateFromKey(key string) (time.Time, bool) {
	// Look for the pattern: /YYYY/MM/DD/HH/
	parts := strings.Split(key, "/")
	// We need at least 4 trailing path segments: YYYY, MM, DD, HH, filename
	for i := 0; i+4 < len(parts); i++ {
		if len(parts[i]) == 4 && len(parts[i+1]) == 2 && len(parts[i+2]) == 2 {
			dateStr := parts[i] + "/" + parts[i+1] + "/" + parts[i+2]
			t, err := time.Parse("2006/01/02", dateStr)
			if err == nil {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

// downloadAndParse fetches a single .json.gz file from S3, decompresses it,
// and parses newline-delimited JSON into InvocationLog records.
func (f *Fetcher) downloadAndParse(ctx context.Context, key string) ([]model.InvocationLog, error) {
	output, err := f.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(f.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("downloading: %w", err)
	}
	defer output.Body.Close()

	gz, err := gzip.NewReader(output.Body)
	if err != nil {
		return nil, fmt.Errorf("opening gzip reader: %w", err)
	}
	defer gz.Close()

	return parseNDJSON(gz)
}

// parseNDJSON reads newline-delimited JSON from r and returns the parsed logs.
func parseNDJSON(r io.Reader) ([]model.InvocationLog, error) {
	var logs []model.InvocationLog
	scanner := bufio.NewScanner(r)

	// Bedrock log lines can be large; increase the buffer to 10 MB.
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var entry model.InvocationLog
		if err := json.Unmarshal(line, &entry); err != nil {
			return nil, fmt.Errorf("parsing JSON line: %w", err)
		}
		logs = append(logs, entry)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading lines: %w", err)
	}

	return logs, nil
}
