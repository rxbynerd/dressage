package conversation

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/rxbynerd/dressage/internal/model"
)

// openaiResponse is the relevant subset of a non-streaming Chat Completions
// response: {choices:[{message, finish_reason}], usage:{...}}.
type openaiResponse struct {
	Choices []openaiChoice `json:"choices"`
	Usage   *openaiUsage   `json:"usage"`
}

type openaiChoice struct {
	Message      openaiMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

type openaiUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// openaiChunk is one element of a streaming response array
// (object == "chat.completion.chunk"): {choices:[{delta, finish_reason}]}.
type openaiChunk struct {
	Choices []openaiChunkChoice `json:"choices"`
	Usage   *openaiUsage        `json:"usage"`
}

type openaiChunkChoice struct {
	Delta        openaiDelta `json:"delta"`
	FinishReason string      `json:"finish_reason"`
}

type openaiDelta struct {
	Content   string                `json:"content"`
	ToolCalls []openaiDeltaToolCall `json:"tool_calls"`
}

type openaiDeltaToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// reassembleOpenAIOutput parses a response body and reconstructs the final
// assistant turn. It supports BOTH a single aggregated response object
// (choices[0].message) AND a streaming chunk array (deltas aggregated by index).
func reassembleOpenAIOutput(body json.RawMessage) *model.Turn {
	if len(body) == 0 {
		return nil
	}

	// Try a streaming chunk array first (an array of chat.completion.chunk).
	var chunks []openaiChunk
	if err := json.Unmarshal(body, &chunks); err == nil && len(chunks) > 0 {
		if turn := aggregateOpenAIChunks(chunks); turn != nil {
			return turn
		}
	}

	// Otherwise treat it as a single response object.
	resp := parseOpenAIResponse(body)
	if resp == nil || len(resp.Choices) == 0 {
		return nil
	}
	msg := resp.Choices[0].Message
	msg.Role = "assistant"
	blocks := assistantBlocks(msg.Content, msg.ToolCalls, msg.FunctionCall)
	if len(blocks) == 0 {
		return nil
	}
	return &model.Turn{Role: "assistant", Blocks: blocks}
}

// aggregateOpenAIChunks assembles a Turn from a streaming chunk array:
// content deltas are concatenated; tool_call deltas are assembled by index with
// their argument strings concatenated.
func aggregateOpenAIChunks(chunks []openaiChunk) *model.Turn {
	var textBuf strings.Builder

	type toolBuilder struct {
		id      string
		name    string
		argsBuf strings.Builder
	}
	builders := make(map[int]*toolBuilder)

	for _, chunk := range chunks {
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta
		textBuf.WriteString(delta.Content)

		for _, tc := range delta.ToolCalls {
			b := builders[tc.Index]
			if b == nil {
				b = &toolBuilder{}
				builders[tc.Index] = b
			}
			if tc.ID != "" {
				b.id = tc.ID
			}
			if tc.Function.Name != "" {
				b.name = tc.Function.Name
			}
			b.argsBuf.WriteString(tc.Function.Arguments)
		}
	}

	turn := &model.Turn{Role: "assistant"}
	if text := textBuf.String(); text != "" {
		turn.Blocks = append(turn.Blocks, model.ContentBlock{Type: "text", Text: text})
	}

	indices := make([]int, 0, len(builders))
	for idx := range builders {
		indices = append(indices, idx)
	}
	sort.Ints(indices)
	for _, idx := range indices {
		b := builders[idx]
		turn.Blocks = append(turn.Blocks, model.ContentBlock{
			Type:      "tool_use",
			ToolName:  b.name,
			ToolID:    b.id,
			ToolInput: prettyJSON(json.RawMessage(b.argsBuf.String())),
		})
	}

	if len(turn.Blocks) == 0 {
		return nil
	}
	return turn
}

// parseOpenAIResponse decodes a single non-streaming response object. Returns
// nil on empty/invalid input.
func parseOpenAIResponse(body json.RawMessage) *openaiResponse {
	if len(body) == 0 {
		return nil
	}
	var resp openaiResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil
	}
	return &resp
}
