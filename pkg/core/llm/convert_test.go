package llm

import (
	"reflect"
	"testing"

	"github.com/smallnest/matex/pkg/core/errs"
)

// Convert must translate any wire request shape into any other, through the
// canonical Request, without losing messages, tool calls or results.

func TestConvertChatToAnthropic(t *testing.T) {
	chat := []byte(`{"model":"m","messages":[
		{"role":"system","content":"be brief"},
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"beijing\"}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"sunny"}]}`)
	body, err := Convert(chat, ProtocolChat, ProtocolAnthropic)
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, body)
	if m["system"] != "be brief" {
		t.Fatalf("system = %v", m["system"])
	}
	if m["max_tokens"] == nil {
		t.Fatal("anthropic max_tokens must always be present")
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3", len(msgs))
	}
	assistant := msgs[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if assistant["type"] != "tool_use" || assistant["id"] != "c1" {
		t.Fatalf("tool_use = %v", assistant)
	}
	if assistant["input"].(map[string]any)["city"] != "beijing" {
		t.Fatalf("tool_use.input = %v, want object", assistant["input"])
	}
	last := msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if last["type"] != "tool_result" || last["tool_use_id"] != "c1" || last["content"] != "sunny" {
		t.Fatalf("tool_result = %v", last)
	}
}

func TestConvertAnthropicToChat(t *testing.T) {
	anthropic := []byte(`{"model":"m","max_tokens":128,"system":"be brief","messages":[
		{"role":"user","content":[{"type":"text","text":"hi"}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"get_weather","input":{"city":"beijing"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"sunny"},{"type":"text","text":"thanks"}]}]}`)
	body, err := Convert(anthropic, ProtocolAnthropic, ProtocolChat)
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, body)
	msgs := m["messages"].([]any)
	// system + user + assistant + tool + user(the "thanks" turn) = 5
	if len(msgs) != 5 {
		t.Fatalf("messages = %d, want 5 (system + user + assistant + tool + user)", len(msgs))
	}
	assistant := msgs[2].(map[string]any)
	if assistant["role"] != "assistant" {
		t.Fatalf("role = %v", assistant["role"])
	}
	fn := assistant["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "get_weather" || fn["arguments"] != `{"city":"beijing"}` {
		t.Fatalf("function = %v", fn)
	}
	tool := msgs[3].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "tu1" || tool["content"] != "sunny" {
		t.Fatalf("tool message = %v", tool)
	}
	if msgs[4].(map[string]any)["content"] != "thanks" {
		t.Fatalf("last message = %v, want the user text", msgs[4])
	}
}

func TestConvertRoundTripPreservesRequest(t *testing.T) {
	want := Request{
		Model:  "m",
		System: "be brief",
		Messages: []Message{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "ok", ToolCalls: []ToolCall{{ID: "c1", Name: "get_weather", Arguments: `{"city":"beijing"}`}}},
			{Role: "tool", ToolCallID: "c1", Content: "sunny"},
		},
		Tools:      []Tool{{Name: "get_weather", Description: "weather", Schema: map[string]any{"type": "object"}}},
		ToolChoice: "name:get_weather",
		MaxTokens:  128,
	}
	// chat, responses and anthropic carry every field in the body.
	for _, proto := range []Protocol{ProtocolChat, ProtocolResponses, ProtocolAnthropic} {
		wire, err := adapterForTest(proto).build(want)
		if err != nil {
			t.Fatalf("%s build: %v", proto, err)
		}
		got, err := adapterForTest(proto).parseRequest(wire)
		if err != nil {
			t.Fatalf("%s parse: %v", proto, err)
		}
		if !reflect.DeepEqual(*got, want) {
			t.Errorf("%s round trip:\n got %+v\nwant %+v", proto, *got, want)
		}
	}
	// Gemini's body carries no model and no call id on tool results; those two
	// losses are inherent to the wire, everything else survives.
	wire, err := adapterForTest(ProtocolGemini).build(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := adapterForTest(ProtocolGemini).parseRequest(wire)
	if err != nil {
		t.Fatal(err)
	}
	if got.System != want.System || got.ToolChoice != want.ToolChoice || got.MaxTokens != want.MaxTokens {
		t.Fatalf("gemini round trip basics: %+v", got)
	}
	if !reflect.DeepEqual(got.Messages[0], want.Messages[0]) {
		t.Fatalf("gemini user message: %+v", got.Messages[0])
	}
	if got.Messages[1].Content != want.Messages[1].Content || got.Messages[1].Role != "assistant" {
		t.Fatalf("gemini assistant message: %+v", got.Messages[1])
	}
	c := got.Messages[1].ToolCalls[0]
	if c.Name != "get_weather" || c.Arguments != `{"city":"beijing"}` {
		t.Fatalf("gemini tool call: %+v", c)
	}
	if got.Messages[2].Role != "tool" || got.Messages[2].Content != "sunny" || got.Messages[2].Name != "get_weather" {
		t.Fatalf("gemini tool result: %+v", got.Messages[2])
	}
	if !reflect.DeepEqual(got.Tools, want.Tools) {
		t.Fatalf("gemini tools: %+v", got.Tools)
	}
}

func TestConvertUnsupportedProtocol(t *testing.T) {
	_, err := Convert([]byte(`{}`), "bogus", ProtocolChat)
	if errs.KindOf(err) != errs.KindInvalidArgument {
		t.Fatalf("err = %v, want KindInvalidArgument", err)
	}
}

func TestGeminiFunctionResponseNameFromCall(t *testing.T) {
	// The tool result has no Name; the resolver must find get_weather from the
	// assistant's call with the same id.
	r := Request{
		Messages: []Message{
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "get_weather", Arguments: `{}`}}},
			{Role: "tool", ToolCallID: "c1", Content: "sunny"},
		},
	}
	m := decode(t, conv(t, r.ToGemini))
	fr := m["contents"].([]any)[1].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionResponse"].(map[string]any)
	if fr["name"] != "get_weather" {
		t.Fatalf("functionResponse.name = %v, want get_weather", fr["name"])
	}
}

func TestRawObjectWrapsInvalidJSON(t *testing.T) {
	if got := string(rawObject(`{bad`)); got != `{"input":"{bad"}` {
		t.Fatalf("invalid args wrapped as %s", got)
	}
	if got := string(rawObject(`{"city":"x"}`)); got != `{"city":"x"}` {
		t.Fatalf("valid args = %s", got)
	}
	if got := string(rawObject(``)); got != `{}` {
		t.Fatalf("empty args = %s", got)
	}
}

func adapterForTest(p Protocol) adapter { return adapters[p] }
