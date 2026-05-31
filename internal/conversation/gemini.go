package conversation

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/rxbynerd/dressage/internal/model"
)

// Gemini (Vertex AI generateContent) envelope reconstruction.
//
// Gemini requests use the Google generative-ai envelope rather than Anthropic
// Messages: a top-level `contents` array of {role, parts}, with `systemInstruction`,
// `tools[].functionDeclarations`, and `generationConfig` alongside it. Parts are
// heterogeneous (text, functionCall, functionResponse, inlineData, fileData) and
// a "thinking" part is a normal text part carrying `thought: true` rather than a
// distinct type.
//
// Streaming responses are logged as multiple GenerateContentResponse chunks;
// vertexfetch wraps a multi-chunk response into a JSON array, which
// reassembleGeminiOutput detects and aggregates (concatenating streamed text and
// taking usage/finishReason from the final chunk).

// geminiRequest is the subset of a Vertex generateContent request body Dressage reconstructs.
type geminiRequest struct {
	Contents          []geminiContent `json:"contents"`
	SystemInstruction *geminiContent  `json:"systemInstruction"`
	Tools             []geminiTool    `json:"tools"`
}

type geminiContent struct {
	Role  string       `json:"role"` // "user" or "model"
	Parts []geminiPart `json:"parts"`
}

// geminiPart is one element of a content's parts[]. Exactly one payload field is
// populated per part; `thought` is a boolean flag on an otherwise-text part.
type geminiPart struct {
	Text             string              `json:"text"`
	Thought          bool                `json:"thought"`
	FunctionCall     *geminiFunctionCall `json:"functionCall"`
	FunctionResponse *geminiFunctionResp `json:"functionResponse"`
	InlineData       *geminiInlineData   `json:"inlineData"`
	FileData         *geminiFileData     `json:"fileData"`
}

type geminiFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type geminiFunctionResp struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

type geminiInlineData struct {
	MimeType string `json:"mimeType"`
}

type geminiFileData struct {
	MimeType string `json:"mimeType"`
	FileURI  string `json:"fileUri"`
}

type geminiTool struct {
	FunctionDeclarations []geminiFunctionDecl `json:"functionDeclarations"`
}

type geminiFunctionDecl struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// geminiResponse is the subset of a GenerateContentResponse Dressage reads.
type geminiResponse struct {
	Candidates    []geminiCandidate `json:"candidates"`
	UsageMetadata *geminiUsage      `json:"usageMetadata"`
}

type geminiCandidate struct {
	Content      geminiContent `json:"content"`
	FinishReason string        `json:"finishReason"`
}

type geminiUsage struct {
	PromptTokenCount        int64 `json:"promptTokenCount"`
	CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
	CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
}

// parsedGeminiInvocation pairs a record with its parsed Gemini request body.
type parsedGeminiInvocation struct {
	rec *model.Record
	req *geminiRequest
}

// reconstructGemini builds a ConversationDetail from records belonging to one
// conversation, decoded as Vertex generateContent request/response bodies. It
// mirrors reconstructAnthropic: find the invocation with the most contents (the
// fullest history), expand its request into turns, append the final model
// response from its output, and attach per-invocation metrics to earlier turns.
func reconstructGemini(records []model.Record) *model.ConversationDetail {
	if len(records) == 0 {
		return nil
	}

	sorted := make([]model.Record, len(records))
	copy(sorted, records)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Timestamp.Before(sorted[j].Timestamp)
	})

	var all []parsedGeminiInvocation
	bestIdx := -1
	for i := range sorted {
		req := parseGeminiRequest(sorted[i].Input.JSON)
		if req == nil {
			continue
		}
		all = append(all, parsedGeminiInvocation{rec: &sorted[i], req: req})
		if bestIdx < 0 || len(req.Contents) > len(all[bestIdx].req.Contents) {
			bestIdx = len(all) - 1
		}
	}

	if bestIdx < 0 {
		return nil
	}
	// Resolve the pointer AFTER the loop so it does not alias into a slice that
	// may have been reallocated by append.
	best := &all[bestIdx]

	detail := &model.ConversationDetail{
		SessionID:    extractSessionFromSystemInstruction(best.req.SystemInstruction),
		SystemPrompt: geminiSystemPrompt(best.req.SystemInstruction),
		Tools:        geminiTools(best.req.Tools),
	}

	// Expand each content into a turn (model → assistant), preserving a strict
	// 1:1 content↔turn mapping. attachGeminiMetrics relies on that invariant:
	// it places an invocation's assistant turn at index = number of contents in
	// its request, which only holds if no content is dropped here. (Contents
	// always carry parts in practice; a block-less turn is rare and renders
	// harmlessly empty rather than silently shifting later turn indices.)
	for _, c := range best.req.Contents {
		detail.Turns = append(detail.Turns, geminiTurn(c))
	}

	// Append the final model response from the output body.
	finalTurn, stopReason, usage := reassembleGeminiOutput(best.rec.Output.JSON)
	if finalTurn != nil && len(finalTurn.Blocks) > 0 {
		finalTurn.Metrics = geminiMetrics(best.rec, stopReason, usage)
		detail.Turns = append(detail.Turns, *finalTurn)
	}

	attachGeminiMetrics(detail, all)

	return detail
}

// attachGeminiMetrics correlates earlier invocations with the assistant turns
// they produced. Because reconstructGemini maps every content 1:1 to a turn in
// order, the assistant turn produced by an invocation sits at index = number of
// contents in its request.
func attachGeminiMetrics(detail *model.ConversationDetail, invocations []parsedGeminiInvocation) {
	for _, p := range invocations {
		turnIdx := len(p.req.Contents)
		if turnIdx >= len(detail.Turns) {
			continue // final invocation, already handled
		}
		turn := &detail.Turns[turnIdx]
		if turn.Role != "assistant" || turn.Metrics != nil {
			continue
		}
		_, stopReason, usage := reassembleGeminiOutput(p.rec.Output.JSON)
		turn.Metrics = geminiMetrics(p.rec, stopReason, usage)
	}
}

// geminiMetrics builds TurnMetrics from the record plus the response usage and
// finishReason. Gemini reports cache reads but no cache-write counter, so
// CacheWriteTokens stays 0.
func geminiMetrics(rec *model.Record, stopReason string, usage *geminiUsage) *model.TurnMetrics {
	m := &model.TurnMetrics{
		Timestamp:       rec.Timestamp,
		RequestID:       rec.RequestID,
		ModelID:         rec.ModelID,
		InputTokens:     rec.Input.TokenCount,
		OutputTokens:    rec.Output.TokenCount,
		CacheReadTokens: rec.Input.CacheRead,
		LatencyMs:       rec.LatencyMs,
		StopReason:      stopReason,
	}
	if usage != nil {
		if usage.PromptTokenCount > 0 {
			m.InputTokens = usage.PromptTokenCount
		}
		if usage.CandidatesTokenCount > 0 {
			m.OutputTokens = usage.CandidatesTokenCount
		}
		if usage.CachedContentTokenCount > 0 {
			m.CacheReadTokens = usage.CachedContentTokenCount
		}
	}
	return m
}

func parseGeminiRequest(body json.RawMessage) *geminiRequest {
	if len(body) == 0 {
		return nil
	}
	var req geminiRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}
	if len(req.Contents) == 0 {
		return nil
	}
	return &req
}

// geminiSystemPrompt flattens the systemInstruction text parts.
func geminiSystemPrompt(si *geminiContent) string {
	if si == nil {
		return ""
	}
	var sb []string
	for _, p := range si.Parts {
		if p.Text != "" {
			sb = append(sb, p.Text)
		}
	}
	return joinNonEmpty(sb, "\n\n")
}

// geminiTools maps tools[].functionDeclarations to model.ToolDef, truncating long
// descriptions for display (matching the other reconstructors).
func geminiTools(tools []geminiTool) []model.ToolDef {
	var result []model.ToolDef
	for _, t := range tools {
		for _, fd := range t.FunctionDeclarations {
			desc := fd.Description
			if len(desc) > 200 {
				desc = desc[:200] + "..."
			}
			result = append(result, model.ToolDef{Name: fd.Name, Description: desc})
		}
	}
	return result
}

// geminiTurn converts one content into a Turn. The Gemini "model" role maps to
// Dressage's "assistant"; "user" stays "user" (functionResponse parts ride in
// user turns, consistent with how tool_result blocks are placed elsewhere).
func geminiTurn(c geminiContent) model.Turn {
	role := c.Role
	if role == "model" {
		role = "assistant"
	}
	if role == "" {
		role = "user"
	}
	return model.Turn{Role: role, Blocks: partsToBlocks(c.Parts)}
}

// partsToBlocks converts Gemini parts into content blocks, coalescing adjacent
// text (and adjacent thinking) parts so streamed fragments render as one block.
// Non-text parts (tool calls/results, media placeholders) reset the coalescing
// anchor so a media placeholder is never glued onto the text that follows it.
func partsToBlocks(parts []geminiPart) []model.ContentBlock {
	var blocks []model.ContentBlock
	mergeIdx := -1 // index of the block that may absorb more text of its type, or -1

	appendText := func(typ, text string) {
		if text == "" {
			return
		}
		if mergeIdx >= 0 && blocks[mergeIdx].Type == typ {
			blocks[mergeIdx].Text += text
			return
		}
		blocks = append(blocks, model.ContentBlock{Type: typ, Text: text})
		mergeIdx = len(blocks) - 1
	}
	appendBlock := func(b model.ContentBlock) {
		blocks = append(blocks, b)
		mergeIdx = -1 // a non-coalescible block breaks the text run
	}

	for _, p := range parts {
		switch {
		case p.FunctionCall != nil:
			appendBlock(model.ContentBlock{
				Type:      "tool_use",
				ToolName:  p.FunctionCall.Name,
				ToolInput: prettyJSON(p.FunctionCall.Args),
			})
		case p.FunctionResponse != nil:
			// Gemini has no tool-call id; surface the function name as the ToolID
			// so the report labels the result ("Result for <name>").
			appendBlock(model.ContentBlock{
				Type:          "tool_result",
				ToolID:        p.FunctionResponse.Name,
				ResultContent: prettyJSON(p.FunctionResponse.Response),
			})
		case p.InlineData != nil:
			appendBlock(model.ContentBlock{Type: "text", Text: inlineDataPlaceholder(p.InlineData)})
		case p.FileData != nil:
			appendBlock(model.ContentBlock{Type: "text", Text: fileDataPlaceholder(p.FileData)})
		case p.Thought:
			// A thinking part is a text part flagged thought:true.
			appendText("thinking", p.Text)
		case p.Text != "":
			appendText("text", p.Text)
		}
	}
	return blocks
}

// inlineDataPlaceholder renders a non-text inline part as a labeled text block.
// Dressage's ContentBlock taxonomy has no media type yet (planned), so inline
// images/audio/PDFs are surfaced as a placeholder rather than dropped.
func inlineDataPlaceholder(d *geminiInlineData) string {
	if d.MimeType != "" {
		return fmt.Sprintf("[inline data: %s]", d.MimeType)
	}
	return "[inline data]"
}

func fileDataPlaceholder(d *geminiFileData) string {
	switch {
	case d.FileURI != "" && d.MimeType != "":
		return fmt.Sprintf("[file: %s (%s)]", d.FileURI, d.MimeType)
	case d.FileURI != "":
		return fmt.Sprintf("[file: %s]", d.FileURI)
	case d.MimeType != "":
		return fmt.Sprintf("[file: %s]", d.MimeType)
	default:
		return "[file]"
	}
}

// reassembleGeminiOutput reconstructs the final assistant turn from a response
// body that is either a single GenerateContentResponse or a JSON array of
// streamed chunks. For a streamed array it concatenates parts across all chunks
// (first candidate) and takes the finishReason/usageMetadata from the last chunk
// that carries them.
func reassembleGeminiOutput(outputBody json.RawMessage) (*model.Turn, string, *geminiUsage) {
	if len(outputBody) == 0 {
		return nil, "", nil
	}

	// Streamed: array of chunks.
	var chunks []geminiResponse
	if err := json.Unmarshal(outputBody, &chunks); err == nil && len(chunks) > 0 {
		var parts []geminiPart
		var stopReason string
		var usage *geminiUsage
		for i := range chunks {
			if len(chunks[i].Candidates) > 0 {
				parts = append(parts, chunks[i].Candidates[0].Content.Parts...)
				if fr := chunks[i].Candidates[0].FinishReason; fr != "" {
					stopReason = fr
				}
			}
			if chunks[i].UsageMetadata != nil {
				usage = chunks[i].UsageMetadata
			}
		}
		return &model.Turn{Role: "assistant", Blocks: partsToBlocks(parts)}, stopReason, usage
	}

	// Non-streaming: single response object.
	var resp geminiResponse
	if err := json.Unmarshal(outputBody, &resp); err != nil || len(resp.Candidates) == 0 {
		return nil, "", nil
	}
	cand := resp.Candidates[0]
	return &model.Turn{Role: "assistant", Blocks: partsToBlocks(cand.Content.Parts)}, cand.FinishReason, resp.UsageMetadata
}

// extractSessionFromSystemInstruction looks for the "_session_" marker inside the
// systemInstruction text. Gemini has no first-class metadata.user_id equivalent,
// so a wrapping harness that wants session grouping must embed the marker there
// (documented in docs/providers/vertex.md). Returns "" when absent.
func extractSessionFromSystemInstruction(si *geminiContent) string {
	if si == nil {
		return ""
	}
	for _, p := range si.Parts {
		if sid := sessionSuffix(p.Text); sid != "" {
			return sid
		}
	}
	return ""
}

// extractSessionGemini parses a Gemini request body and returns the session id
// embedded in the systemInstruction text, if any.
func extractSessionGemini(inputBody json.RawMessage) string {
	if len(inputBody) == 0 {
		return ""
	}
	var req struct {
		SystemInstruction *geminiContent `json:"systemInstruction"`
	}
	if err := json.Unmarshal(inputBody, &req); err != nil {
		return ""
	}
	return extractSessionFromSystemInstruction(req.SystemInstruction)
}

// joinNonEmpty joins the non-empty elements of parts with sep.
func joinNonEmpty(parts []string, sep string) string {
	out := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		if out != "" {
			out += sep
		}
		out += p
	}
	return out
}
