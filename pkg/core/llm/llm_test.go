package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/smallnest/matex/pkg/core/errs"
)

// The conversions are the wire contract: what a canonical Request becomes on
// each protocol. These tests pin the JSON shapes, so a change to a field name
// or a merge rule fails loudly.

func decode(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode wire body %s: %v", body, err)
	}
	return m
}

func TestToChat(t *testing.T) {
	temp := 0.7
	r := Request{
		Model:       "m",
		System:      "be brief",
		MaxTokens:   128,
		Temperature: &temp,
		ToolChoice:  "name:get_weather",
		Messages: []Message{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: "c1", Name: "get_weather", Arguments: `{"city":"beijing"}`}}},
			{Role: "tool", ToolCallID: "c1", Content: "sunny"},
		},
		Tools: []Tool{{Name: "get_weather", Description: "weather", Schema: map[string]any{"type": "object"}}},
	}
	m := decode(t, conv(t, r.ToChat))
	if m["model"] != "m" || m["max_tokens"] != float64(128) {
		t.Fatalf("model/max_tokens = %v/%v", m["model"], m["max_tokens"])
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages len = %d, want 4 (system+user+assistant+tool)", len(msgs))
	}
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("first message is not system: %v", msgs[0])
	}
	tool := msgs[2].(map[string]any)
	if tool["role"] != "assistant" {
		t.Fatalf("third message is not assistant: %v", tool)
	}
	calls := tool["tool_calls"].([]any)
	call := calls[0].(map[string]any)
	if call["id"] != "c1" || call["type"] != "function" {
		t.Fatalf("tool call envelope = %v", call)
	}
	fn := call["function"].(map[string]any)
	if fn["name"] != "get_weather" || fn["arguments"] != `{"city":"beijing"}` {
		t.Fatalf("function = %v", fn)
	}
	toolMsg := msgs[3].(map[string]any)
	if toolMsg["tool_call_id"] != "c1" || toolMsg["content"] != "sunny" {
		t.Fatalf("tool message = %v", toolMsg)
	}
	tc := m["tool_choice"].(map[string]any)
	if tc["type"] != "function" || tc["function"].(map[string]any)["name"] != "get_weather" {
		t.Fatalf("tool_choice = %v", tc)
	}
}

func TestToChatStreamFlag(t *testing.T) {
	r := Request{Model: "m", Stream: true, Messages: []Message{{Role: "user", Content: "hi"}}}
	m := decode(t, conv(t, r.ToChat))
	if m["stream"] != true {
		t.Fatalf("stream = %v, want true", m["stream"])
	}
	if _, ok := m["stream_options"]; !ok {
		t.Fatal("stream_options must be set so usage is included")
	}
}

func TestToResponses(t *testing.T) {
	r := Request{
		Model:  "m",
		System: "be brief",
		Messages: []Message{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: "c1", Name: "get_weather", Arguments: `{"city":"beijing"}`}}},
			{Role: "tool", ToolCallID: "c1", Content: "sunny"},
		},
		Tools: []Tool{{Name: "get_weather", Description: "weather"}},
	}
	m := decode(t, conv(t, r.ToResponses))
	if m["instructions"] != "be brief" || m["store"] != false {
		t.Fatalf("instructions/store = %v/%v", m["instructions"], m["store"])
	}
	input := m["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("input len = %d, want 3", len(input))
	}
	call := input[1].(map[string]any)
	if call["type"] != "function_call" || call["call_id"] != "c1" || call["arguments"] != `{"city":"beijing"}` {
		t.Fatalf("function_call item = %v", call)
	}
	out := input[2].(map[string]any)
	if out["type"] != "function_call_output" || out["call_id"] != "c1" || out["output"] != "sunny" {
		t.Fatalf("function_call_output item = %v", out)
	}
	tools := m["tools"].([]any)
	if tools[0].(map[string]any)["name"] != "get_weather" {
		t.Fatalf("tools = %v", tools)
	}
}

func TestToAnthropic(t *testing.T) {
	r := Request{
		Model:     "m",
		System:    "be brief",
		MaxTokens: 0, // must be defaulted: the API requires max_tokens
		Messages: []Message{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: "c1", Name: "get_weather", Arguments: `{"city":"beijing"}`}}},
			{Role: "tool", ToolCallID: "c1", Content: "sunny"},
			{Role: "user", Content: "thanks"},
		},
		Tools: []Tool{{Name: "get_weather", Description: "weather"}},
	}
	m := decode(t, conv(t, r.ToAnthropic))
	if m["max_tokens"] != float64(defaultAnthropicMaxTokens) {
		t.Fatalf("max_tokens = %v, want %d", m["max_tokens"], defaultAnthropicMaxTokens)
	}
	if m["system"] != "be brief" {
		t.Fatalf("system = %v", m["system"])
	}
	msgs := m["messages"].([]any)
	// user, assistant(tool_use), then ONE user message carrying the tool_result
	// and the following "thanks" text merged.
	if len(msgs) != 3 {
		t.Fatalf("messages len = %d, want 3 (tool result must merge with next user turn)", len(msgs))
	}
	assistant := msgs[1].(map[string]any)
	blocks := assistant["content"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("assistant content blocks = %d, want 1", len(blocks))
	}
	use := blocks[0].(map[string]any)
	if use["type"] != "tool_use" || use["id"] != "c1" || use["name"] != "get_weather" {
		t.Fatalf("tool_use block = %v", use)
	}
	if got := use["input"].(map[string]any)["city"]; got != "beijing" {
		t.Fatalf("tool_use input = %v, want object with city=beijing", use["input"])
	}
	last := msgs[2].(map[string]any)
	lastBlocks := last["content"].([]any)
	if len(lastBlocks) != 2 {
		t.Fatalf("merged user message blocks = %d, want 2 (tool_result + text)", len(lastBlocks))
	}
	tr := lastBlocks[0].(map[string]any)
	if tr["type"] != "tool_result" || tr["tool_use_id"] != "c1" || tr["content"] != "sunny" {
		t.Fatalf("tool_result block = %v", tr)
	}
	tools := m["tools"].([]any)
	if tools[0].(map[string]any)["input_schema"] == nil {
		t.Fatalf("tool missing input_schema: %v", tools[0])
	}
}

func TestToAnthropicToolChoice(t *testing.T) {
	tools := []Tool{{Name: "get_weather", Description: "weather"}}
	r := Request{Model: "m", ToolChoice: "required", Tools: tools, Messages: []Message{{Role: "user", Content: "hi"}}}
	if tc := decode(t, conv(t, r.ToAnthropic))["tool_choice"]; tc != "any" {
		t.Fatalf("required maps to %v, want \"any\"", tc)
	}
	r.ToolChoice = "name:get_weather"
	got := decode(t, conv(t, r.ToAnthropic))["tool_choice"].(map[string]any)
	if got["type"] != "tool" || got["name"] != "get_weather" {
		t.Fatalf("name: tool_choice = %v", got)
	}
}

func TestToGemini(t *testing.T) {
	r := Request{
		Model:  "m",
		System: "be brief",
		Messages: []Message{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: "c1", Name: "get_weather", Arguments: `{"city":"beijing"}`}}},
			{Role: "tool", Name: "get_weather", ToolCallID: "c1", Content: "sunny"},
		},
		Tools:       []Tool{{Name: "get_weather", Description: "weather"}},
		Temperature: ptr(0.5),
		MaxTokens:   64,
	}
	m := decode(t, conv(t, r.ToGemini))
	si := m["systemInstruction"].(map[string]any)
	if si["parts"].([]any)[0].(map[string]any)["text"] != "be brief" {
		t.Fatalf("systemInstruction = %v", si)
	}
	contents := m["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("contents len = %d, want 3", len(contents))
	}
	if contents[0].(map[string]any)["role"] != "user" || contents[1].(map[string]any)["role"] != "model" {
		t.Fatalf("roles = %v / %v", contents[0].(map[string]any)["role"], contents[1].(map[string]any)["role"])
	}
	call := contents[1].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionCall"].(map[string]any)
	if call["name"] != "get_weather" {
		t.Fatalf("functionCall = %v", call)
	}
	if call["args"].(map[string]any)["city"] != "beijing" {
		t.Fatalf("functionCall args = %v, want object", call["args"])
	}
	fr := contents[2].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionResponse"].(map[string]any)
	if fr["name"] != "get_weather" {
		t.Fatalf("functionResponse = %v (needs the function name)", fr)
	}
	if fr["response"].(map[string]any)["output"] != "sunny" {
		t.Fatalf("functionResponse response = %v", fr["response"])
	}
	gen := m["generationConfig"].(map[string]any)
	if gen["temperature"] != 0.5 || gen["maxOutputTokens"] != float64(64) {
		t.Fatalf("generationConfig = %v", gen)
	}
	decls := m["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)
	if decls[0].(map[string]any)["name"] != "get_weather" {
		t.Fatalf("functionDeclarations = %v", decls)
	}
}

func TestToGeminiToolConfig(t *testing.T) {
	r := Request{Model: "m", ToolChoice: "name:get_weather"}
	cfg := decode(t, conv(t, r.ToGemini))["toolConfig"].(map[string]any)["functionCallingConfig"].(map[string]any)
	if cfg["mode"] != "ANY" || cfg["allowedFunctionNames"].([]any)[0] != "get_weather" {
		t.Fatalf("functionCallingConfig = %v", cfg)
	}
}

// ---- reply parsing -----------------------------------------------------------

func TestParseChat(t *testing.T) {
	body := []byte(`{"id":"r1","model":"m","choices":[{"message":{"role":"assistant","content":"hi there","tool_calls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"beijing\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	got, err := ParseChat(body)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "r1" || got.Content != "hi there" || got.FinishReason != "tool_calls" {
		t.Fatalf("parse = %+v", got)
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].Name != "get_weather" || got.ToolCalls[0].Arguments != `{"city":"beijing"}` {
		t.Fatalf("tool calls = %+v", got.ToolCalls)
	}
	if got.Usage != (Usage{10, 5, 15}) {
		t.Fatalf("usage = %+v", got.Usage)
	}
}

func TestParseResponses(t *testing.T) {
	body := []byte(`{"id":"r1","model":"m","status":"completed","output":[
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]},
		{"type":"function_call","id":"fc1","name":"get_weather","arguments":"{\"city\":\"beijing\"}"}],
		"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`)
	got, err := ParseResponses(body)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "ok" || got.FinishReason != "tool_calls" {
		t.Fatalf("parse = %+v", got)
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].ID != "fc1" || got.ToolCalls[0].Arguments != `{"city":"beijing"}` {
		t.Fatalf("tool calls = %+v", got.ToolCalls)
	}
	if got.Usage != (Usage{10, 5, 15}) {
		t.Fatalf("usage = %+v", got.Usage)
	}
}

func TestParseResponsesIncomplete(t *testing.T) {
	body := []byte(`{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}`)
	got, err := ParseResponses(body)
	if err != nil {
		t.Fatal(err)
	}
	if got.FinishReason != "length" {
		t.Fatalf("finish = %q, want length", got.FinishReason)
	}
}

func TestParseAnthropic(t *testing.T) {
	body := []byte(`{"id":"r1","model":"m","stop_reason":"tool_use","content":[
		{"type":"text","text":"ok"},
		{"type":"tool_use","id":"tu1","name":"get_weather","input":{"city":"beijing"}}],
		"usage":{"input_tokens":10,"output_tokens":5}}`)
	got, err := ParseAnthropic(body)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "ok" || got.FinishReason != "tool_calls" {
		t.Fatalf("parse = %+v", got)
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].ID != "tu1" || got.ToolCalls[0].Arguments != `{"city":"beijing"}` {
		t.Fatalf("tool calls = %+v", got.ToolCalls)
	}
	if got.Usage != (Usage{10, 5, 15}) { // total = input + output
		t.Fatalf("usage = %+v", got.Usage)
	}
}

func TestParseGemini(t *testing.T) {
	body := []byte(`{"candidates":[{"content":{"role":"model","parts":[
		{"text":"ok"},
		{"functionCall":{"name":"get_weather","args":{"city":"beijing"}}}]},
		"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}`)
	got, err := ParseGemini(body)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "ok" || got.FinishReason != "stop" {
		t.Fatalf("parse = %+v", got)
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].Name != "get_weather" || got.ToolCalls[0].Arguments != `{"city":"beijing"}` {
		t.Fatalf("tool calls = %+v", got.ToolCalls)
	}
	if got.Usage != (Usage{10, 5, 15}) {
		t.Fatalf("usage = %+v", got.Usage)
	}
}

func TestFinishNormalization(t *testing.T) {
	cases := []struct {
		proto string
		raw   string
		want  string
	}{
		{"chat", "length", "length"},
		{"chat", "function_call", "tool_calls"},
		{"anthropic", "end_turn", "stop"},
		{"anthropic", "max_tokens", "length"},
		{"anthropic", "refusal", "refusal"},
		{"gemini", "MAX_TOKENS", "length"},
		{"gemini", "SAFETY", "content_filter"},
	}
	for _, c := range cases {
		var got string
		switch c.proto {
		case "chat":
			got = chatFinish(c.raw)
		case "anthropic":
			got = anthropicFinish(c.raw)
		case "gemini":
			got = geminiFinish(c.raw)
		}
		if got != c.want {
			t.Errorf("%s finish %q = %q, want %q", c.proto, c.raw, got, c.want)
		}
	}
}

// ---- end to end: non-streaming ------------------------------------------------

func TestChatHappyPath(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_, _ = fmt.Fprint(w, `{"id":"r1","model":"m","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`)
	}))
	defer srv.Close()

	c := New(WithBaseURL(srv.URL), WithAPIKey("k"), WithModel("m"), WithTimeout(0))
	resp, err := c.Chat(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/chat/completions" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer k" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if resp.Content != "hi" || resp.FinishReason != "stop" || resp.Usage.TotalTokens != 3 {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestChatToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"id":"r1","model":"m","choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"beijing\"}"}}]},"finish_reason":"tool_calls"}],"usage":{}}`)
	}))
	defer srv.Close()

	c := New(WithBaseURL(srv.URL), WithModel("m"))
	resp, err := c.Chat(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason != "tool_calls" || len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "get_weather" {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestChatHTTPErrorMapping(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   errs.Kind
	}{
		{400, `{"error":{"message":"bad"}}`, errs.KindInvalidArgument},
		{401, `{"error":{"message":"no key"}}`, errs.KindUnauthorized},
		{404, `{"error":{"message":"gone"}}`, errs.KindNotFound},
		{429, `{"error":{"message":"slow down"}}`, errs.KindRateLimited},
		{500, `{"error":{"message":"boom"}}`, errs.KindUnavailable},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			_, _ = fmt.Fprint(w, c.body)
		}))
		cl := New(WithBaseURL(srv.URL), WithModel("m"))
		_, err := cl.Chat(context.Background(), Request{})
		srv.Close()
		if errs.KindOf(err) != c.want {
			t.Errorf("status %d: got kind %v, want %v (err: %v)", c.status, errs.KindOf(err), c.want, err)
		}
	}
}

func TestChatProtocolInference(t *testing.T) {
	cases := []struct {
		base string
		want Protocol
	}{
		{"https://api.openai.com/v1", ProtocolChat},
		{"https://api.deepseek.com", ProtocolChat},
		{"https://api.anthropic.com", ProtocolAnthropic},
		{"https://generativelanguage.googleapis.com/v1beta", ProtocolGemini},
	}
	for _, c := range cases {
		if got := inferProtocol(c.base); got != c.want {
			t.Errorf("inferProtocol(%q) = %q, want %q", c.base, got, c.want)
		}
	}
	// WithProtocol overrides inference.
	if got := New(WithBaseURL("https://api.anthropic.com"), WithProtocol(ProtocolChat)).Protocol(); got != ProtocolChat {
		t.Fatalf("forced protocol = %q", got)
	}
}

func TestChatUnsupportedProtocol(t *testing.T) {
	c := New(WithBaseURL("http://x"), WithProtocol("bogus"))
	_, err := c.Chat(context.Background(), Request{})
	if errs.KindOf(err) != errs.KindInvalidArgument {
		t.Fatalf("err = %v, want KindInvalidArgument", err)
	}
}

// ---- end to end: streaming ------------------------------------------------------

// drain collects all events until the channel closes; a missing EventDone is a
// failure. The EventDone is the last element, so its Stop is events' final word.
func drain(t *testing.T, ch <-chan Event) []Event {
	t.Helper()
	var out []Event
	for ev := range ch {
		out = append(out, ev)
	}
	if len(out) == 0 || out[len(out)-1].Kind != EventDone {
		t.Fatalf("stream must end with EventDone, got %+v", out)
	}
	return out
}

func collectText(evs []Event) string {
	var b strings.Builder
	for _, ev := range evs {
		if ev.Kind == EventText {
			b.WriteString(ev.Text)
		}
	}
	return b.String()
}

func collectCalls(evs []Event) []ToolCall {
	var out []ToolCall
	for _, ev := range evs {
		if ev.Kind == EventToolCall {
			out = append(out, ev.ToolCall)
		}
	}
	return out
}

func TestChatStream(t *testing.T) {
	srv := sseServer(t, strings.Join([]string{
		sseData(`{"choices":[{"delta":{"content":"Hel"}}]}`),
		sseData(`{"choices":[{"delta":{"content":"lo"}}]}`),
		sseData(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"get_weather","arguments":"{\"city\":"}}]}}]}`),
		sseData(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"beijing\"}"}}]}}]}`),
		sseData(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`),
		sseData(`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`),
		sseData("[DONE]"),
	}, ""))
	defer srv.Close()

	c := New(WithBaseURL(srv.URL), WithModel("m"))
	ch, err := c.ChatStream(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	evs := drain(t, ch)
	if got := collectText(evs); got != "Hello" {
		t.Fatalf("text = %q", got)
	}
	calls := collectCalls(evs)
	if len(calls) != 1 || calls[0].ID != "c1" || calls[0].Name != "get_weather" || calls[0].Arguments != `{"city":"beijing"}` {
		t.Fatalf("tool calls = %+v", calls)
	}
	last := evs[len(evs)-1]
	if last.Stop != "tool_calls" {
		t.Fatalf("stop = %q, want tool_calls", last.Stop)
	}
}

func TestAnthropicStream(t *testing.T) {
	events := strings.Join([]string{
		sseData(`{"type":"message_start","message":{"id":"msg1","model":"m","usage":{"input_tokens":10}}}`),
		sseData(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		sseData(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`),
		sseData(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`),
		sseData(`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu1","name":"get_weather","input":{}}}`),
		sseData(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`),
		sseData(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"beijing\"}"}}`),
		sseData(`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`),
		sseData(`{"type":"message_stop"}`),
	}, "")
	srv := sseServer(t, events)
	defer srv.Close()

	c := New(WithBaseURL(srv.URL), WithAPIKey("k"), WithProtocol(ProtocolAnthropic), WithModel("m"))
	ch, err := c.ChatStream(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	evs := drain(t, ch)
	if got := collectText(evs); got != "Hello" {
		t.Fatalf("text = %q", got)
	}
	calls := collectCalls(evs)
	if len(calls) != 1 || calls[0].ID != "tu1" || calls[0].Name != "get_weather" || calls[0].Arguments != `{"city":"beijing"}` {
		t.Fatalf("tool calls = %+v", calls)
	}
	if evs[len(evs)-1].Stop != "tool_calls" {
		t.Fatalf("stop = %q", evs[len(evs)-1].Stop)
	}
}

func TestResponsesStream(t *testing.T) {
	events := strings.Join([]string{
		sseData(`{"type":"response.created","response":{"id":"r1","model":"m"}}`),
		sseData(`{"type":"response.output_text.delta","item_id":"msg1","output_index":0,"content_index":0,"delta":"Hel"}`),
		sseData(`{"type":"response.output_text.delta","item_id":"msg1","output_index":0,"content_index":0,"delta":"lo"}`),
		sseData(`{"type":"response.completed","response":{"status":"completed","output":[{"type":"function_call","id":"fc1","name":"get_weather","arguments":"{\"city\":\"beijing\"}"}],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`),
	}, "")
	srv := sseServer(t, events)
	defer srv.Close()

	c := New(WithBaseURL(srv.URL), WithProtocol(ProtocolResponses), WithModel("m"))
	ch, err := c.ChatStream(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	evs := drain(t, ch)
	if got := collectText(evs); got != "Hello" {
		t.Fatalf("text = %q", got)
	}
	calls := collectCalls(evs)
	if len(calls) != 1 || calls[0].ID != "fc1" || calls[0].Arguments != `{"city":"beijing"}` {
		t.Fatalf("tool calls = %+v", calls)
	}
	if evs[len(evs)-1].Stop != "tool_calls" {
		t.Fatalf("stop = %q", evs[len(evs)-1].Stop)
	}
}

func TestGeminiStream(t *testing.T) {
	events := strings.Join([]string{
		sseData(`{"candidates":[{"content":{"parts":[{"text":"Hel"}]}}]}`),
		sseData(`{"candidates":[{"content":{"parts":[{"text":"lo"}]}}]}`),
		sseData(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"get_weather","args":{"city":"beijing"}}}]}}]}`),
		sseData(`{"candidates":[{"content":{"parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}`),
	}, "")
	srv := sseServer(t, events)
	defer srv.Close()

	c := New(WithBaseURL(srv.URL), WithAPIKey("k"), WithProtocol(ProtocolGemini), WithModel("m"))
	ch, err := c.ChatStream(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	evs := drain(t, ch)
	if got := collectText(evs); got != "Hello" {
		t.Fatalf("text = %q", got)
	}
	calls := collectCalls(evs)
	if len(calls) != 1 || calls[0].Name != "get_weather" || calls[0].Arguments != `{"city":"beijing"}` {
		t.Fatalf("tool calls = %+v", calls)
	}
	if evs[len(evs)-1].Stop != "stop" {
		t.Fatalf("stop = %q", evs[len(evs)-1].Stop)
	}
}

func TestGeminiStreamUsesStreamEndpoint(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		// stream until the client aborts, then stop
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseData(`{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`)))
		fl, _ := w.(http.Flusher)
		fl.Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := New(WithBaseURL(srv.URL), WithProtocol(ProtocolGemini), WithModel("m"))
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := c.ChatStream(ctx, Request{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	<-ch     // one text event
	cancel() // abort
	<-ch     // stream closes with an error or done
	if gotPath != "/models/m:streamGenerateContent" {
		t.Fatalf("path = %q", gotPath)
	}
}

// ---- helpers ---------------------------------------------------------------------

func sseData(s string) string { return "data: " + s + "\n\n" }

func sseServer(t *testing.T, events string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, events)
	}))
}

func conv(t *testing.T, f func() ([]byte, error)) []byte {
	t.Helper()
	b, err := f()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func ptr[T any](v T) *T { return &v }

// A stream that keeps flowing must outlive http.Client.Timeout (which, left
// on the client, would cut it mid-body); the timeout still guards
// time-to-first-byte.
func TestStreamIsNotCutByClientTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := range 5 {
			_, _ = fmt.Fprint(w, sseData(`{"choices":[{"delta":{"content":"x"}}]}`))
			w.(http.Flusher).Flush()
			if i < 4 {
				time.Sleep(60 * time.Millisecond)
			}
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := New(WithBaseURL(srv.URL), WithModel("m"), WithTimeout(100*time.Millisecond))
	ch, err := c.ChatStream(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer StopStream(ch)
	ticks := 0
	start := time.Now()
	for ev := range ch {
		switch ev.Kind {
		case EventText:
			ticks++
		case EventError:
			t.Fatalf("stream cut after %d chunks at %v (~300ms > 100ms timeout): %v", ticks, time.Since(start), ev.Err)
		}
	}
	if ticks != 5 {
		t.Fatalf("received %d of 5 chunks", ticks)
	}
}

func TestStreamStillTimesOutWaitingForHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()

	c := New(WithBaseURL(srv.URL), WithModel("m"), WithTimeout(100*time.Millisecond))
	start := time.Now()
	_, err := c.ChatStream(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("a server that never sends headers must time out")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("waited %v for headers", time.Since(start))
	}
}
