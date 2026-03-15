// Package model defines the data types for AWS Bedrock model invocation logs.
package model

import (
	"encoding/json"
	"time"
)

// InvocationLog represents a single Bedrock model invocation log entry
// as stored in S3 by AWS Bedrock's model invocation logging feature.
type InvocationLog struct {
	SchemaType    string          `json:"schemaType"`
	SchemaVersion string          `json:"schemaVersion"`
	Timestamp     time.Time       `json:"timestamp"`
	AccountID     string          `json:"accountId"`
	Region        string          `json:"region"`
	RequestID     string          `json:"requestId"`
	Operation     string          `json:"operation"`
	ModelID       string          `json:"modelId"`
	Status        string          `json:"status"`
	ErrorCode     string          `json:"errorCode,omitempty"`
	Identity      Identity        `json:"identity"`
	Input         InvocationInput `json:"input"`
	Output        InvocationOutput `json:"output"`
	Performance   *PerformanceConfig `json:"performanceConfig,omitempty"`
}

// Identity contains the AWS identity that made the invocation.
type Identity struct {
	ARN string `json:"arn"`
}

// InvocationInput contains the request payload and metadata.
type InvocationInput struct {
	InputBodyJSON             json.RawMessage `json:"inputBodyJson"`
	InputContentType          string          `json:"inputContentType"`
	InputTokenCount           int64           `json:"inputTokenCount"`
	CacheReadInputTokenCount  int64           `json:"cacheReadInputTokenCount,omitempty"`
	CacheWriteInputTokenCount int64           `json:"cacheWriteInputTokenCount,omitempty"`
}

// InvocationOutput contains the response payload and metadata.
type InvocationOutput struct {
	OutputBodyJSON  json.RawMessage `json:"outputBodyJson"`
	OutputContentType string        `json:"outputContentType"`
	OutputTokenCount  int64         `json:"outputTokenCount"`
}

// PerformanceConfig contains performance-related metadata.
type PerformanceConfig struct {
	Latency string `json:"latency"`
}
