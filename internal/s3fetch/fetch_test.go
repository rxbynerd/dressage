package s3fetch

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// countingS3 records GetObject calls so tests can assert that the bucket-
// confinement guard short-circuits before any network call is made.
type countingS3 struct {
	getCalls int
	body     string // returned as the GetObject body (must be non-empty to read)
}

func (c *countingS3) ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	return &s3.ListObjectsV2Output{}, nil
}

func (c *countingS3) GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	c.getCalls++
	return &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader(c.body))}, nil
}

func TestParseS3URI(t *testing.T) {
	cases := []struct {
		name       string
		uri        string
		wantBucket string
		wantKey    string
		wantErr    bool
	}{
		{"simple", "s3://my-bucket/path/to/obj.json.gz", "my-bucket", "path/to/obj.json.gz", false},
		{"no leading slash trimmed", "s3://b/k", "b", "k", false},
		{"wrong scheme", "https://my-bucket/k", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bucket, key, err := parseS3URI(tc.uri)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseS3URI(%q) expected error, got none", tc.uri)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseS3URI(%q): %v", tc.uri, err)
			}
			if bucket != tc.wantBucket {
				t.Errorf("bucket = %q, want %q", bucket, tc.wantBucket)
			}
			if key != tc.wantKey {
				t.Errorf("key = %q, want %q", key, tc.wantKey)
			}
		})
	}
}

// A crafted s3:// path naming a different bucket must be refused without any
// GetObject call.
func TestFetchS3PathRejectsForeignBucket(t *testing.T) {
	client := &countingS3{}
	f := New(nil, "trusted-bucket", "prefix")
	f.client = client

	_, err := f.fetchS3Path(context.Background(), "s3://attacker-bucket/secret.json.gz")
	if err == nil {
		t.Fatal("expected error fetching from a foreign bucket, got nil")
	}
	if !strings.Contains(err.Error(), "attacker-bucket") {
		t.Errorf("error %q should name the rejected bucket", err)
	}
	if client.getCalls != 0 {
		t.Errorf("GetObject called %d times, want 0 (guard must short-circuit)", client.getCalls)
	}
}

// A path naming the configured bucket passes the guard (and proceeds to fetch).
func TestFetchS3PathAllowsConfiguredBucket(t *testing.T) {
	client := &countingS3{body: `{"ok":true}`}
	f := New(nil, "trusted-bucket", "prefix")
	f.client = client

	data, err := f.fetchS3Path(context.Background(), "s3://trusted-bucket/payload.json")
	if err != nil {
		t.Fatalf("fetchS3Path: %v", err)
	}
	if string(data) != `{"ok":true}` {
		t.Errorf("data = %s, want {\"ok\":true}", data)
	}
	if client.getCalls != 1 {
		t.Errorf("GetObject called %d times, want 1", client.getCalls)
	}
}

// A single garbage NDJSON line must be skipped, not abort the whole parse.
func TestParseNDJSONSkipsMalformedLine(t *testing.T) {
	const good = `{"requestId":"req-good","modelId":"claude-3"}`
	input := good + "\n" + "this is not json\n"

	logs, err := parseNDJSON(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parseNDJSON: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("logs = %d, want 1 (good line kept, garbage skipped)", len(logs))
	}
	if logs[0].RequestID != "req-good" {
		t.Errorf("RequestID = %q, want req-good", logs[0].RequestID)
	}
}
