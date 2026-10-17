package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// chatAdapter speaks the OpenAI Chat Completions API.
type chatAdapter struct{}

func (chatAdapter) path(Request) string { return "/chat/completions" }

func (chatAdapter) headers(c *Client) http.Header {
	h := http.Header{}
	if c.apiKey != "" {
		h.Set("Authorization", "Bearer "+c.apiKey)
	}
	return h
}

func (chatAdapter) build(req Request) ([]byte, error) { return req.ToChat() }

func (chatAdapter) parseRequest(body []byte) (*Request, error) { return parseChatRequest(body) }

func (chatAdapter) parse(body []byte) (*Response, error) { return ParseChat(body) }

// stream decodes chat's SSE chunks. Tool call arguments arrive split across
// chunks per call index and are joined before each call is emitted.
func (chatAdapter) stream(ctx context.Context, s *sse, out chan<- Event) {
	var pending = map[int]*toolBuf{}
	var order []int
	var usage Usage
	stop := ""
	for {
		data, ok, err := s.next()
		if err != nil {
			if err == io.EOF {
				break
			}
			send(ctx, out, Event{Kind: EventError, Err: fmt.Errorf("llm: chat stream: %w", err)})
			return
		}
		if !ok {
			break
		}
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			send(ctx, out, Event{Kind: EventError, Err: fmt.Errorf("llm: chat chunk: %w", err)})
			return
		}
		if len(chunk.Choices) > 0 {
			d := chunk.Choices[0].Delta
			if d.Content != "" {
				if !send(ctx, out, Event{Kind: EventText, Text: d.Content}) {
					return
				}
			}
			for _, tc := range d.ToolCalls {
				b := pending[tc.Index]
				if b == nil {
					b = &toolBuf{}
					pending[tc.Index] = b
					order = append(order, tc.Index)
				}
				if tc.ID != "" {
					b.id = tc.ID
				}
				if tc.Function.Name != "" {
					b.name = tc.Function.Name
				}
				b.args += tc.Function.Arguments
			}
			if chunk.Choices[0].FinishReason != "" {
				stop = chunk.Choices[0].FinishReason
			}
		}
		if chunk.Usage != nil {
			usage = Usage{InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens, TotalTokens: chunk.Usage.TotalTokens}
		}
	}
	for _, i := range order {
		b := pending[i]
		if !send(ctx, out, Event{Kind: EventToolCall, ToolCall: ToolCall{ID: b.id, Name: b.name, Arguments: b.args}}) {
			return
		}
	}
	if !zeroUsage(usage) {
		send(ctx, out, Event{Kind: EventUsage, Usage: usage})
	}
	send(ctx, out, Event{Kind: EventDone, Stop: chatFinish(stop)})
}
