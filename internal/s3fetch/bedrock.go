package s3fetch

import (
	"encoding/json"
	"time"

	"github.com/rxbynerd/dressage/internal/model"
)

// bedrockLog represents a single Bedrock model invocation log entry as stored
// in S3 by AWS Bedrock's model invocation logging feature. It is the on-the-wire
// shape; normalize() converts it to a provider-neutral model.Record.
type bedrockLog struct {
	SchemaType    string             `json:"schemaType"`
	SchemaVersion string             `json:"schemaVersion"`
	Timestamp     time.Time          `json:"timestamp"`
	AccountID     string             `json:"accountId"`
	Region        string             `json:"region"`
	RequestID     string             `json:"requestId"`
	Operation     string             `json:"operation"`
	ModelID       string             `json:"modelId"`
	Status        string             `json:"status"`
	ErrorCode     string             `json:"errorCode,omitempty"`
	Identity      bedrockIdentity    `json:"identity"`
	Input         bedrockInput       `json:"input"`
	Output        bedrockOutput      `json:"output"`
	Performance   *bedrockPerfConfig `json:"performanceConfig,omitempty"`
}

// bedrockIdentity contains the AWS identity that made the invocation.
type bedrockIdentity struct {
	ARN string `json:"arn"`
}

// bedrockInput contains the request payload and metadata. When the input body
// exceeds 100KB, AWS stores it in a separate S3 object and populates
// InputBodyS3Path instead of InputBodyJSON.
type bedrockInput struct {
	InputBodyJSON             json.RawMessage `json:"inputBodyJson"`
	InputBodyS3Path           string          `json:"inputBodyS3Path,omitempty"`
	InputContentType          string          `json:"inputContentType"`
	InputTokenCount           int64           `json:"inputTokenCount"`
	CacheReadInputTokenCount  int64           `json:"cacheReadInputTokenCount,omitempty"`
	CacheWriteInputTokenCount int64           `json:"cacheWriteInputTokenCount,omitempty"`
}

// bedrockOutput contains the response payload and metadata. When the output
// body exceeds 100KB, AWS stores it in a separate S3 object and populates
// OutputBodyS3Path instead of OutputBodyJSON.
type bedrockOutput struct {
	OutputBodyJSON    json.RawMessage `json:"outputBodyJson"`
	OutputBodyS3Path  string          `json:"outputBodyS3Path,omitempty"`
	OutputContentType string          `json:"outputContentType"`
	OutputTokenCount  int64           `json:"outputTokenCount"`
}

// bedrockPerfConfig contains performance-related metadata.
type bedrockPerfConfig struct {
	Latency string `json:"latency"`
}

// normalize converts a Bedrock log entry into a provider-neutral model.Record.
func (l bedrockLog) normalize() model.Record {
	var extra map[string]string
	if l.AccountID != "" || l.Region != "" {
		extra = map[string]string{}
		if l.AccountID != "" {
			extra["accountId"] = l.AccountID
		}
		if l.Region != "" {
			extra["region"] = l.Region
		}
	}
	return model.Record{
		Provider:  "bedrock",
		Timestamp: l.Timestamp,
		RequestID: l.RequestID,
		ModelID:   l.ModelID,
		Operation: l.Operation,
		Status:    l.Status,
		ErrorCode: l.ErrorCode,
		Identity:  model.Identity{Principal: l.Identity.ARN, Extra: extra},
		Input: model.Body{
			JSON:        l.Input.InputBodyJSON,
			ContentType: l.Input.InputContentType,
			TokenCount:  l.Input.InputTokenCount,
			CacheRead:   l.Input.CacheReadInputTokenCount,
			CacheWrite:  l.Input.CacheWriteInputTokenCount,
		},
		Output: model.Body{
			JSON:        l.Output.OutputBodyJSON,
			ContentType: l.Output.OutputContentType,
			TokenCount:  l.Output.OutputTokenCount,
		},
	}
}
