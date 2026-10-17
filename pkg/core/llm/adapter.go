package llm

import (
	"context"
	"net/http"
)

// adapter is one wire protocol's slice of the client: where to POST, how to
// authorize, and how requests and replies map to and from the canonical shape.
type adapter interface {
	// path is the endpoint appended to the base URL.
	path(req Request) string
	headers(c *Client) http.Header
	// build renders a canonical Request as the protocol's wire body.
	build(req Request) ([]byte, error)
	// parseRequest reads the protocol's request body back into a canonical
	// Request (used by Convert).
	parseRequest(body []byte) (*Request, error)
	// parse turns a non-streaming reply into a canonical Response.
	parse(body []byte) (*Response, error)
	// stream decodes the SSE reply into canonical events.
	stream(ctx context.Context, s *sse, out chan<- Event)
}

// adapters maps each protocol to its implementation.
var adapters = map[Protocol]adapter{
	ProtocolChat:      chatAdapter{},
	ProtocolResponses: responsesAdapter{},
	ProtocolAnthropic: anthropicAdapter{},
	ProtocolGemini:    geminiAdapter{},
}
