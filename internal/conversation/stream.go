package conversation

import (
	"encoding/json"
	"strings"

	"github.com/rubynerd/dressage/internal/model"
)

// streamChunk represents a single SSE-like chunk from a Bedrock streaming response.
type streamChunk struct {
	Type         string          `json:"type"`
	Index        int             `json:"index"`
	Message      json.RawMessage `json:"message,omitempty"`
	ContentBlock json.RawMessage `json:"content_block,omitempty"`
	Delta        json.RawMessage `json:"delta,omitempty"`
	Usage        json.RawMessage `json:"usage,omitempty"`
	Metrics      json.RawMessage `json:"amazon-bedrock-invocationMetrics,omitempty"`
}

type contentBlockInfo struct {
	Type string `json:"type"` // "thinking", "text", "tool_use"
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

type contentDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
}

type messageDelta struct {
	StopReason string `json:"stop_reason"`
}

type bedrockMetrics struct {
	InputTokenCount           int64 `json:"inputTokenCount"`
	OutputTokenCount          int64 `json:"outputTokenCount"`
	InvocationLatency         int64 `json:"invocationLatency"`
	FirstByteLatency          int64 `json:"firstByteLatency"`
	CacheReadInputTokenCount  int64 `json:"cacheReadInputTokenCount"`
	CacheWriteInputTokenCount int64 `json:"cacheWriteInputTokenCount"`
}

// streamMetrics holds extracted metrics from the streaming output.
type streamMetrics struct {
	StopReason       string
	LatencyMs        int64
	FirstByteMs      int64
	CacheReadTokens  int64
	CacheWriteTokens int64
}

// reassembleOutput parses a streaming response body (JSON array of chunks)
// and reconstructs the assistant turn with all content blocks.
// Also handles non-streaming responses (single JSON object).
func reassembleOutput(body json.RawMessage) *model.Turn {
	if len(body) == 0 {
		return nil
	}

	// Try as a streaming response (JSON array of chunks).
	var chunks []streamChunk
	if err := json.Unmarshal(body, &chunks); err == nil && len(chunks) > 0 {
		return reassembleChunks(chunks)
	}

	// Try as a non-streaming response (single JSON object with content array).
	return parseNonStreamingResponse(body)
}

// reassembleChunks builds a Turn from an array of streaming SSE chunks.
func reassembleChunks(chunks []streamChunk) *model.Turn {
	// Track content blocks being built up.
	type blockBuilder struct {
		info    contentBlockInfo
		textBuf strings.Builder
	}
	builders := make(map[int]*blockBuilder)

	var stopReason string

	for _, chunk := range chunks {
		switch chunk.Type {
		case "content_block_start":
			var info contentBlockInfo
			if err := json.Unmarshal(chunk.ContentBlock, &info); err == nil {
				builders[chunk.Index] = &blockBuilder{info: info}
			}

		case "content_block_delta":
			b := builders[chunk.Index]
			if b == nil {
				continue
			}
			var delta contentDelta
			if err := json.Unmarshal(chunk.Delta, &delta); err != nil {
				continue
			}
			switch delta.Type {
			case "text_delta":
				b.textBuf.WriteString(delta.Text)
			case "thinking_delta":
				b.textBuf.WriteString(delta.Thinking)
			case "input_json_delta":
				b.textBuf.WriteString(delta.PartialJSON)
			case "signature_delta":
				// Ignore signature deltas (thinking block signatures).
			}

		case "message_delta":
			var md messageDelta
			if err := json.Unmarshal(chunk.Delta, &md); err == nil {
				stopReason = md.StopReason
			}
		}
	}

	// Build the turn from assembled content blocks.
	turn := &model.Turn{Role: "assistant"}

	// Process blocks in index order.
	maxIdx := -1
	for idx := range builders {
		if idx > maxIdx {
			maxIdx = idx
		}
	}

	for i := 0; i <= maxIdx; i++ {
		b := builders[i]
		if b == nil {
			continue
		}
		text := b.textBuf.String()

		switch b.info.Type {
		case "text":
			if text != "" {
				turn.Blocks = append(turn.Blocks, model.ContentBlock{
					Type: "text",
					Text: text,
				})
			}
		case "thinking":
			if text != "" {
				turn.Blocks = append(turn.Blocks, model.ContentBlock{
					Type: "thinking",
					Text: text,
				})
			}
		case "tool_use":
			turn.Blocks = append(turn.Blocks, model.ContentBlock{
				Type:      "tool_use",
				ToolName:  b.info.Name,
				ToolID:    b.info.ID,
				ToolInput: prettyJSON(json.RawMessage(text)),
			})
		}
	}

	// Attach stop reason as a simple metric hint (full metrics attached separately).
	if stopReason != "" && len(turn.Blocks) > 0 {
		turn.Metrics = &model.TurnMetrics{StopReason: stopReason}
	}

	if len(turn.Blocks) == 0 {
		return nil
	}
	return turn
}

// parseNonStreamingResponse handles a non-streaming Messages API response.
func parseNonStreamingResponse(body json.RawMessage) *model.Turn {
	var resp struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		StopReason string          `json:"stop_reason"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil
	}
	if resp.Role == "" {
		return nil
	}

	msg := apiMessage{Role: resp.Role, Content: resp.Content}
	turn := convertMessage(msg)
	if len(turn.Blocks) == 0 {
		return nil
	}
	if resp.StopReason != "" {
		turn.Metrics = &model.TurnMetrics{StopReason: resp.StopReason}
	}
	return &turn
}

// extractStreamMetrics parses the streaming output to extract performance metrics
// from the message_stop chunk.
func extractStreamMetrics(body json.RawMessage) *streamMetrics {
	if len(body) == 0 {
		return nil
	}

	var chunks []streamChunk
	if err := json.Unmarshal(body, &chunks); err != nil {
		return nil
	}

	sm := &streamMetrics{}

	for _, chunk := range chunks {
		switch chunk.Type {
		case "message_delta":
			var md messageDelta
			if err := json.Unmarshal(chunk.Delta, &md); err == nil {
				sm.StopReason = md.StopReason
			}
		case "message_stop":
			if len(chunk.Metrics) > 0 {
				var bm bedrockMetrics
				if err := json.Unmarshal(chunk.Metrics, &bm); err == nil {
					sm.LatencyMs = bm.InvocationLatency
					sm.FirstByteMs = bm.FirstByteLatency
					sm.CacheReadTokens = bm.CacheReadInputTokenCount
					sm.CacheWriteTokens = bm.CacheWriteInputTokenCount
				}
			}
		case "message_start":
			// Extract cache info from initial usage if available.
			if len(chunk.Message) > 0 {
				var msg struct {
					Usage struct {
						CacheReadInputTokens    int64 `json:"cache_read_input_tokens"`
						CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
					} `json:"usage"`
				}
				if err := json.Unmarshal(chunk.Message, &msg); err == nil {
					if msg.Usage.CacheReadInputTokens > 0 {
						sm.CacheReadTokens = msg.Usage.CacheReadInputTokens
					}
					if msg.Usage.CacheCreationInputTokens > 0 {
						sm.CacheWriteTokens = msg.Usage.CacheCreationInputTokens
					}
				}
			}
		}
	}

	if sm.StopReason == "" && sm.LatencyMs == 0 {
		return nil
	}
	return sm
}
