// Package llm is the unified LLM client of matex.
//
// It speaks four wire protocols behind one canonical request/response shape:
// OpenAI Chat Completions, OpenAI Responses, Anthropic Messages and Google
// Gemini generateContent. Requests are written once in the canonical form and
// converted to whichever protocol a vendor serves; replies come back in one
// shape again. Business code uses llm.Client instead of vendor SDKs.
//
// Beside them, Client.Decide speaks the System One API a Jev (decision) model
// answers: typed questions (noul / score / choice / probability) with how
// likely each option was, as TypeSafe's Jev, Vercel's AI Gateway or
// Cloudflare's Workers AI serve it. System One has its own canonical types
// (DecideRequest / DecideResponse); it is not part of the four-protocol
// conversion set.
//
// A Client is built with functional options. The protocol defaults to the one
// inferred from BaseURL and can be forced with WithProtocol. The host
// heuristic knows only the named vendors; for anything else set WithProtocol
// yourself — no probe request is ever sent, and the unknown host defaults to
// OpenAI Chat Completions, the standard most gateways speak. Streaming is a
// separate call, ChatStream, which emits canonical events.
//
// Not in v1: multimodal content, embeddings, Gemini countTokens, decision
// routing groups.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/smallnest/matex/pkg/core/errs"
)

// Protocol is a wire API the client can speak.
type Protocol string

const (
	// ProtocolChat is the OpenAI Chat Completions API.
	ProtocolChat Protocol = "chat"
	// ProtocolResponses is the OpenAI Responses API.
	ProtocolResponses Protocol = "responses"
	// ProtocolAnthropic is the Anthropic Messages API.
	ProtocolAnthropic Protocol = "anthropic"
	// ProtocolGemini is the Google Gemini generateContent API.
	ProtocolGemini Protocol = "gemini"
	// ProtocolJev is TypeSafe's System One API, which Jev answers.
	ProtocolJev Protocol = "jev"
)

// JevVia is how a decision API is asked: TypeSafe's System One shape, Vercel's
// AI Gateway evaluation models, or Cloudflare's Workers AI envelope.
type JevVia string

const (
	// JevViaSystemOne posts the state and questions as they are to
	// {base}/systemone — how TypeSafe's System One and magpie gateways serve it.
	JevViaSystemOne JevVia = "systemone"
	// JevViaVercelEval posts to {base}/evaluation-model: a noul is asked as a
	// boolean, the model rides in the ai-model-id header, and its answers are
	// returned as System One gives them.
	JevViaVercelEval JevVia = "vercel-eval"
	// JevViaCloudflare posts to {base}/ai/run, base being the account URL
	// (e.g. https://api.cloudflare.com/client/v4/accounts/<id>, no /ai): the
	// state and questions ride in a {model, input} envelope, the answer in the
	// result field.
	JevViaCloudflare JevVia = "cloudflare"
)

// Message is one turn of a conversation, protocol-independent.
type Message struct {
	// Role is one of "system", "user", "assistant" or "tool".
	Role string
	// Content is the message text.
	Content string
	// ToolCallID is set on a tool message: the id of the call it answers.
	ToolCallID string
	// Name is set on a tool message: the called function's name. Gemini's
	// functionResponse needs it; other protocols ignore it.
	Name string
	// ToolCalls is set on an assistant message: the calls the model made.
	ToolCalls []ToolCall
}

// ToolCall is a function call the model made.
type ToolCall struct {
	ID   string
	Name string
	// Arguments is the call's JSON-object arguments, as a compact JSON
	// string. Anthropic and Gemini carry it as an object on the wire and
	// are converted here.
	Arguments string
}

// Tool is a function the model may call.
type Tool struct {
	Name        string
	Description string
	// Schema is the JSON schema of the arguments (a map), nil to omit.
	Schema any
}

// Request is a call to a model, protocol-independent.
type Request struct {
	Model string
	// System is the system prompt, sent as its own field by every protocol.
	System   string
	Messages []Message
	Tools    []Tool
	// ToolChoice is "auto", "none", "required" or "name:<tool>"; "" is auto.
	ToolChoice  string
	MaxTokens   int
	Temperature *float64
	TopP        *float64
	Stop        []string
	// Stream is set by ChatStream; Chat forces it off. The conversions honor it.
	Stream bool
}

// Usage counts tokens of a reply.
type Usage struct {
	InputTokens  int
	OutputTokens int
	TotalTokens  int
}

// Response is a non-streaming reply, protocol-independent.
type Response struct {
	ID        string
	Model     string
	Content   string
	ToolCalls []ToolCall
	// FinishReason is normalized: "stop", "length", "tool_calls",
	// "content_filter", "refusal" or "other"; "" when unknown.
	FinishReason string
	Usage        Usage
}

// EventKind is what one streamed reply event carries.
type EventKind int

const (
	// EventText carries a delta of the reply text in Event.Text.
	EventText EventKind = iota
	// EventToolCall carries one complete function call in Event.ToolCall.
	EventToolCall
	// EventUsage carries cumulative token counts in Event.Usage.
	EventUsage
	// EventDone ends a stream; Event.Stop holds the finish reason.
	EventDone
	// EventError ends a stream; Event.Err holds the failure.
	EventError
)

// Event is one thing a streamed reply said.
type Event struct {
	Kind     EventKind
	Text     string
	ToolCall ToolCall
	Usage    Usage
	// Stop is the normalized finish reason, set on EventDone.
	Stop string
	Err  error
}

// defaultTimeout applies when neither WithTimeout nor WithHTTPClient sets one.
const defaultTimeout = 60 * time.Second

// defaultAnthropicMaxTokens is used when an Anthropic request carries none;
// the API requires max_tokens on every call.
const defaultAnthropicMaxTokens = 4096

// Client is a unified LLM client for one base URL and one protocol.
type Client struct {
	baseURL   string
	apiKey    string
	model     string
	protocol  Protocol
	maxTokens int
	jevVia    JevVia
	hc        *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL sets the vendor base URL, e.g. https://api.openai.com/v1 or
// https://generativelanguage.googleapis.com/v1beta.
func WithBaseURL(u string) Option { return func(c *Client) { c.baseURL = strings.TrimRight(u, "/") } }

// WithAPIKey sets the API key (Authorization / x-api-key / x-goog-api-key
// depending on the protocol).
func WithAPIKey(k string) Option { return func(c *Client) { c.apiKey = k } }

// WithModel sets the default model, used when a Request has none.
func WithModel(m string) Option { return func(c *Client) { c.model = m } }

// WithProtocol forces the wire protocol and is the way to name one for a
// provider the host heuristic does not know. Set, it wins over the base URL
// inference (which only recognizes anthropic, gemini and typesafe hosts);
// unset, unknown hosts default to ProtocolChat, the standard most gateways
// speak.
func WithProtocol(p Protocol) Option { return func(c *Client) { c.protocol = p } }

// WithTimeout sets the HTTP client timeout (default 60s). Ignored when
// WithHTTPClient is used.
func WithTimeout(d time.Duration) Option { return func(c *Client) { c.hc = &http.Client{Timeout: d} } }

// WithMaxTokens sets the default max tokens, used when a Request carries none.
func WithMaxTokens(n int) Option { return func(c *Client) { c.maxTokens = n } }

// WithJevVia sets how the System One API is asked: JevViaSystemOne (default),
// JevViaVercelEval or JevViaCloudflare. See Decide.
func WithJevVia(v JevVia) Option { return func(c *Client) { c.jevVia = v } }

// WithHTTPClient sets a custom HTTP client; its Timeout is left untouched.
func WithHTTPClient(hc *http.Client) Option { return func(c *Client) { c.hc = hc } }

// New builds a Client from the options.
func New(opts ...Option) *Client {
	c := &Client{}
	for _, o := range opts {
		o(c)
	}
	if c.hc == nil {
		c.hc = &http.Client{Timeout: defaultTimeout}
	}
	if c.protocol == "" {
		c.protocol = inferProtocol(c.baseURL)
	}
	if c.jevVia == "" {
		c.jevVia = JevViaSystemOne
	}
	return c
}

// Protocol returns the protocol the client speaks.
func (c *Client) Protocol() Protocol { return c.protocol }

// Close releases idle connections. A client passed in via WithHTTPClient is
// left untouched beyond its idle-connection pool.
func (c *Client) Close() error {
	if c.hc != nil {
		c.hc.CloseIdleConnections()
	}
	return nil
}

// Chat sends req and returns the canonical reply.
func (c *Client) Chat(ctx context.Context, req Request) (*Response, error) {
	a, err := c.adapter()
	if err != nil {
		return nil, err
	}
	req = c.fill(req)
	if req.Model == "" {
		return nil, errs.Invalid(0, "llm: model is required (WithModel or Request.Model)")
	}
	req.Stream = false
	body, err := a.build(req)
	if err != nil {
		return nil, errs.InternalWrap(0, err, "llm: build request")
	}
	resp, err := c.do(ctx, a, req, body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, errs.Wrap(errs.KindUnavailable, 0, err, "llm: read reply")
	}
	out, err := a.parse(respBody)
	if err != nil {
		return nil, errs.InternalWrap(0, err, "llm: parse reply")
	}
	return out, nil
}

// ChatStream sends req and returns a channel of canonical events. It returns
// an error only for setup failures; mid-stream failures arrive as EventError.
// Cancel ctx to stop reading early; the body read then errors and the channel
// closes.
func (c *Client) ChatStream(ctx context.Context, req Request) (<-chan Event, error) {
	a, err := c.adapter()
	if err != nil {
		return nil, err
	}
	req = c.fill(req)
	if req.Model == "" {
		return nil, errs.Invalid(0, "llm: model is required (WithModel or Request.Model)")
	}
	req.Stream = true
	body, err := a.build(req)
	if err != nil {
		return nil, errs.InternalWrap(0, err, "llm: build request")
	}
	resp, err := c.do(ctx, a, req, body)
	if err != nil {
		return nil, err
	}
	ch := make(chan Event, 16)
	go func() {
		defer close(ch)
		defer func() { _ = resp.Body.Close() }()
		a.stream(ctx, newSSE(resp.Body), ch)
	}()
	return ch, nil
}

// fill applies the client defaults a Request does not carry.
func (c *Client) fill(req Request) Request {
	if req.Model == "" {
		req.Model = c.model
	}
	if req.MaxTokens <= 0 && c.maxTokens > 0 {
		req.MaxTokens = c.maxTokens
	}
	return req
}

// do POSTs body to the adapter's endpoint and returns the upstream response.
func (c *Client) do(ctx context.Context, a adapter, req Request, body []byte) (*http.Response, error) {
	return c.post(ctx, a.path(req), a.headers(c), body)
}

// post POSTs body to baseURL+path and returns the upstream response on 2xx;
// non-2xx and transport failures map to errs kinds.
func (c *Client) post(ctx context.Context, path string, headers http.Header, body []byte) (*http.Response, error) {
	if c.baseURL == "" {
		return nil, errs.Invalid(0, "llm: base URL is required (WithBaseURL)")
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, errs.InternalWrap(0, err, "llm: new request")
	}
	for k, vs := range headers {
		for _, v := range vs {
			httpReq.Header.Add(k, v)
		}
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return nil, transportErr(err)
	}
	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		return nil, upstreamErr(resp.StatusCode, msg)
	}
	return resp, nil
}

func (c *Client) adapter() (adapter, error) {
	a, ok := adapters[c.protocol]
	if !ok {
		return nil, errs.Invalid(0, "llm: unsupported protocol %q", c.protocol)
	}
	return a, nil
}

// inferProtocol guesses the protocol from a base URL host.
func inferProtocol(base string) Protocol {
	h := strings.ToLower(base)
	switch {
	case strings.Contains(h, "anthropic"):
		return ProtocolAnthropic
	case strings.Contains(h, "generativelanguage"), strings.Contains(h, "googleapis"):
		return ProtocolGemini
	case strings.Contains(h, "typesafe"):
		return ProtocolJev
	default:
		return ProtocolChat
	}
}

// transportErr maps a failed round trip to an errs kind.
func transportErr(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return errs.Wrap(errs.KindTimeout, 0, err, "llm: request timeout")
	}
	return errs.Wrap(errs.KindUnavailable, 0, err, "llm: request failed")
}

// upstreamErr maps an upstream HTTP status to an errs kind, quoting the body.
func upstreamErr(status int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	if msg == "" {
		msg = "(empty body)"
	}
	var kind errs.Kind
	switch {
	case status == http.StatusUnauthorized:
		kind = errs.KindUnauthorized
	case status == http.StatusForbidden:
		kind = errs.KindForbidden
	case status == http.StatusNotFound:
		kind = errs.KindNotFound
	case status == http.StatusTooManyRequests:
		kind = errs.KindRateLimited
	case status >= 500:
		kind = errs.KindUnavailable
	default:
		kind = errs.KindInvalidArgument
	}
	return errs.Newf(kind, 0, "llm: upstream %d: %s", status, msg)
}

// send emits ev, stopping when ctx is cancelled (so an abandoned stream
// cannot leak its goroutine).
func send(ctx context.Context, out chan<- Event, ev Event) bool {
	select {
	case out <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

// compactJSON renders raw as a compact JSON string, "{}" when it is empty or
// invalid — the shape Anthropic and Gemini want for a tool call's arguments.
func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 || !json.Valid(raw) {
		return "{}"
	}
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return "{}"
	}
	return b.String()
}

// rawObject turns a JSON-object string into a json.RawMessage: as-is when it is
// valid JSON, wrapped as {"input": <raw>} when not, so no argument text is lost.
func rawObject(s string) json.RawMessage {
	s = strings.TrimSpace(s)
	if s == "" {
		return json.RawMessage("{}")
	}
	raw := json.RawMessage(s)
	if json.Valid(raw) {
		return raw
	}
	b, _ := json.Marshal(map[string]string{"input": s})
	return b
}

// toolBuf accumulates a streamed tool call's arguments across chunks.
type toolBuf struct {
	id, name, args string
}

// zeroUsage reports whether u is all zero.
func zeroUsage(u Usage) bool { return u.InputTokens == 0 && u.OutputTokens == 0 && u.TotalTokens == 0 }
