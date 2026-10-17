package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// anthropicAdapter speaks the Anthropic Messages API.
type anthropicAdapter struct{}

func (anthropicAdapter) path(Request) string { return "/v1/messages" }

func (anthropicAdapter) headers(c *Client) http.Header {
	h := http.Header{}
	if c.apiKey != "" {
		h.Set("x-api-key", c.apiKey)
	}
	h.Set("anthropic-version", "2023-06-01")
	return h
}

func (anthropicAdapter) build(req Request) ([]byte, error) { return req.ToAnthropic() }

func (anthropicAdapter) parseRequest(body []byte) (*Request, error) {
	return parseAnthropicRequest(body)
}

func (anthropicAdapter) parse(body []byte) (*Response, error) { return ParseAnthropic(body) }

// stream decodes Anthropic SSE events. A tool_use block's arguments stream as
// input_json_delta fragments and are joined before the call is emitted.
func (anthropicAdapter) stream(ctx context.Context, s *sse, out chan<- Event) {
	var pending = map[int]*toolBuf{}
	var order []int
	usage := Usage{}
	stop := ""
	for {
		data, ok, err := s.next()
		if err != nil {
			if err == io.EOF {
				break
			}
			send(ctx, out, Event{Kind: EventError, Err: fmt.Errorf("llm: anthropic stream: %w", err)})
			return
		}
		if !ok {
			break
		}
		var ev struct {
			Type    string `json:"type"`
			Index   *int   `json:"index"`
			Message struct {
				ID    string `json:"id"`
				Model string `json:"model"`
				Usage struct {
					InputTokens int `json:"input_tokens"`
				} `json:"usage"`
			} `json:"message"`
			ContentBlock struct {
				Type  string          `json:"type"`
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			send(ctx, out, Event{Kind: EventError, Err: fmt.Errorf("llm: anthropic event: %w", err)})
			return
		}
		switch ev.Type {
		case "message_start":
			usage.InputTokens = ev.Message.Usage.InputTokens
		case "content_block_start":
			if ev.ContentBlock.Type == "tool_use" && ev.Index != nil {
				idx := *ev.Index
				pending[idx] = &toolBuf{id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
				order = append(order, idx)
			}
		case "content_block_delta":
			switch ev.Delta.Type {
			case "text_delta":
				if ev.Delta.Text != "" {
					if !send(ctx, out, Event{Kind: EventText, Text: ev.Delta.Text}) {
						return
					}
				}
			case "input_json_delta":
				if ev.Index != nil {
					if b := pending[*ev.Index]; b != nil {
						b.args += ev.Delta.PartialJSON
					}
				}
			}
		case "message_delta":
			if ev.Delta.StopReason != "" {
				stop = ev.Delta.StopReason
			}
			if ev.Usage.OutputTokens > 0 {
				usage.OutputTokens = ev.Usage.OutputTokens
			}
		case "error":
			msg := "upstream error"
			if ev.Error != nil && ev.Error.Message != "" {
				msg = ev.Error.Message
			}
			send(ctx, out, Event{Kind: EventError, Err: fmt.Errorf("llm: anthropic: %s", msg)})
			return
		}
	}
	for _, i := range order {
		b := pending[i]
		args := b.args
		if args == "" {
			args = "{}"
		}
		if !send(ctx, out, Event{Kind: EventToolCall, ToolCall: ToolCall{ID: b.id, Name: b.name, Arguments: args}}) {
			return
		}
	}
	usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	if !zeroUsage(usage) {
		send(ctx, out, Event{Kind: EventUsage, Usage: usage})
	}
	send(ctx, out, Event{Kind: EventDone, Stop: anthropicFinish(stop)})
}
