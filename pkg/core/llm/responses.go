package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// responsesAdapter speaks the OpenAI Responses API.
type responsesAdapter struct{}

func (responsesAdapter) path(Request) string { return "/responses" }

func (responsesAdapter) headers(c *Client) http.Header {
	h := http.Header{}
	if c.apiKey != "" {
		h.Set("Authorization", "Bearer "+c.apiKey)
	}
	return h
}

func (responsesAdapter) build(req Request) ([]byte, error) { return req.ToResponses() }

func (responsesAdapter) parseRequest(body []byte) (*Request, error) {
	return parseResponsesRequest(body)
}

func (responsesAdapter) parse(body []byte) (*Response, error) { return ParseResponses(body) }

// stream decodes Responses SSE events. Reply text streams via
// response.output_text.delta; tool calls and usage arrive whole on
// response.completed and are emitted then.
func (responsesAdapter) stream(ctx context.Context, s *sse, out chan<- Event) {
	stop := ""
	usage := Usage{}
	for {
		data, ok, err := s.next()
		if err != nil {
			if err == io.EOF {
				break
			}
			send(ctx, out, Event{Kind: EventError, Err: fmt.Errorf("llm: responses stream: %w", err)})
			return
		}
		if !ok {
			break
		}
		var ev struct {
			Type string `json:"type"`
			// output_text.delta
			Delta string `json:"delta"`
			// response.completed / response.incomplete
			Response struct {
				Status string `json:"status"`
				Output []struct {
					Type      string `json:"type"`
					ID        string `json:"id"`
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"output"`
				IncompleteDetails struct {
					Reason string `json:"reason"`
				} `json:"incomplete_details"`
				Usage struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
					TotalTokens  int `json:"total_tokens"`
				} `json:"usage"`
			} `json:"response"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			send(ctx, out, Event{Kind: EventError, Err: fmt.Errorf("llm: responses event: %w", err)})
			return
		}
		switch ev.Type {
		case "response.output_text.delta":
			if ev.Delta != "" {
				if !send(ctx, out, Event{Kind: EventText, Text: ev.Delta}) {
					return
				}
			}
		case "response.completed":
			hasCalls := false
			for _, it := range ev.Response.Output {
				if it.Type == "function_call" {
					hasCalls = true
					if !send(ctx, out, Event{Kind: EventToolCall, ToolCall: ToolCall{ID: it.ID, Name: it.Name, Arguments: it.Arguments}}) {
						return
					}
				}
			}
			usage = Usage{InputTokens: ev.Response.Usage.InputTokens, OutputTokens: ev.Response.Usage.OutputTokens, TotalTokens: ev.Response.Usage.TotalTokens}
			stop = responsesFinish(ev.Response.Status, ev.Response.IncompleteDetails.Reason, hasCalls)
		case "response.incomplete":
			stop = responsesFinish(ev.Response.Status, ev.Response.IncompleteDetails.Reason, false)
		case "error":
			msg := "upstream error"
			if ev.Error != nil && ev.Error.Message != "" {
				msg = ev.Error.Message
			}
			send(ctx, out, Event{Kind: EventError, Err: fmt.Errorf("llm: responses: %s", msg)})
			return
		}
	}
	if !zeroUsage(usage) {
		send(ctx, out, Event{Kind: EventUsage, Usage: usage})
	}
	send(ctx, out, Event{Kind: EventDone, Stop: stop})
}
