package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// geminiAdapter speaks the Gemini generateContent API. Streaming uses the
// sibling :streamGenerateContent endpoint; its chunks are the same JSON shape
// as a non-streaming reply.
type geminiAdapter struct{}

func (geminiAdapter) path(req Request) string {
	model := strings.TrimPrefix(req.Model, "models/")
	if req.Stream {
		return "/models/" + model + ":streamGenerateContent?alt=sse"
	}
	return "/models/" + model + ":generateContent"
}

func (geminiAdapter) headers(c *Client) http.Header {
	h := http.Header{}
	if c.apiKey != "" {
		h.Set("x-goog-api-key", c.apiKey)
	}
	return h
}

func (geminiAdapter) build(req Request) ([]byte, error) { return req.ToGemini() }

func (geminiAdapter) parseRequest(body []byte) (*Request, error) { return parseGeminiRequest(body) }

func (geminiAdapter) parse(body []byte) (*Response, error) { return ParseGemini(body) }

func (geminiAdapter) stream(ctx context.Context, s *sse, out chan<- Event) {
	usage := Usage{}
	stop := ""
	for {
		data, ok, err := s.next()
		if err != nil {
			if err == io.EOF {
				break
			}
			send(ctx, out, Event{Kind: EventError, Err: fmt.Errorf("llm: gemini stream: %w", err)})
			return
		}
		if !ok {
			break
		}
		var chunk struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text         string `json:"text"`
						FunctionCall struct {
							Name string          `json:"name"`
							Args json.RawMessage `json:"args"`
						} `json:"functionCall"`
					} `json:"parts"`
				} `json:"content"`
				FinishReason string `json:"finishReason"`
			} `json:"candidates"`
			UsageMetadata struct {
				PromptTokenCount     int `json:"promptTokenCount"`
				CandidatesTokenCount int `json:"candidatesTokenCount"`
				TotalTokenCount      int `json:"totalTokenCount"`
			} `json:"usageMetadata"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			send(ctx, out, Event{Kind: EventError, Err: fmt.Errorf("llm: gemini chunk: %w", err)})
			return
		}
		if len(chunk.Candidates) > 0 {
			c := chunk.Candidates[0]
			for _, p := range c.Content.Parts {
				if p.Text != "" {
					if !send(ctx, out, Event{Kind: EventText, Text: p.Text}) {
						return
					}
				}
				if p.FunctionCall.Name != "" {
					if !send(ctx, out, Event{Kind: EventToolCall, ToolCall: ToolCall{Name: p.FunctionCall.Name, Arguments: compactJSON(p.FunctionCall.Args)}}) {
						return
					}
				}
			}
			if c.FinishReason != "" {
				stop = c.FinishReason
			}
		}
		u := Usage{InputTokens: chunk.UsageMetadata.PromptTokenCount, OutputTokens: chunk.UsageMetadata.CandidatesTokenCount, TotalTokens: chunk.UsageMetadata.TotalTokenCount}
		if !zeroUsage(u) {
			usage = u
		}
	}
	if !zeroUsage(usage) {
		send(ctx, out, Event{Kind: EventUsage, Usage: usage})
	}
	send(ctx, out, Event{Kind: EventDone, Stop: geminiFinish(stop)})
}
