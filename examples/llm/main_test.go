package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/smallnest/matex/pkg/core/llm"
)

// The demo must work offline: a fake Chat upstream, a client with functional
// options, and both call styles returning the canned reply.

func TestChatNonStreaming(t *testing.T) {
	srv := newFakeChatServer()
	defer srv.Close()

	c := llm.New(llm.WithBaseURL(srv.URL), llm.WithAPIKey("test"), llm.WithModel("demo-model"), llm.WithTimeout(5*time.Second))
	resp, err := c.Chat(context.Background(), demoRequest())
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "chatcmpl-1" || resp.FinishReason != "stop" {
		t.Fatalf("resp = %+v", resp)
	}
	if !strings.Contains(resp.Content, "散步") {
		t.Fatalf("content = %q", resp.Content)
	}
	if resp.Usage.TotalTokens != 21 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
}

func TestChatStreaming(t *testing.T) {
	srv := newFakeChatServer()
	defer srv.Close()

	c := llm.New(llm.WithBaseURL(srv.URL), llm.WithAPIKey("test"), llm.WithModel("demo-model"))
	ch, err := c.ChatStream(context.Background(), demoRequest())
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	done := false
	for ev := range ch {
		switch ev.Kind {
		case llm.EventText:
			text.WriteString(ev.Text)
		case llm.EventDone:
			done = true
		case llm.EventError:
			t.Fatalf("stream error: %v", ev.Err)
		}
	}
	if !done {
		t.Fatal("stream did not finish with EventDone")
	}
	if got := text.String(); !strings.Contains(got, "适合散步") {
		t.Fatalf("streamed text = %q", got)
	}
}

func TestJevDecide(t *testing.T) {
	srv := newFakeJevServer()
	defer srv.Close()

	c := llm.New(llm.WithBaseURL(srv.URL), llm.WithAPIKey("test"), llm.WithModel("jev-latest"), llm.WithTimeout(5*time.Second))
	out, err := c.Decide(context.Background(), llm.DecideRequest{
		State: map[string]any{"message": "这个查询接口偶尔超时，帮我修一下。"},
		Questions: map[string]llm.Question{
			"intent": {Type: "choice", Criteria: map[string]string{"bug fix": ""}},
			"effort": {Type: "score", Bands: []string{"little", "medium", "much"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Answers["intent"].Choice != "bug fix" || out.Answers["effort"].Score != 1.0 {
		t.Fatalf("answers = %+v", out.Answers)
	}
	if out.Usage.TotalTokens != 52 {
		t.Fatalf("usage = %+v", out.Usage)
	}
}

func TestFourProtocolConversions(t *testing.T) {
	req := demoRequest()
	for _, c := range []struct {
		name   string
		wire   func() ([]byte, error)
		hasKey string
	}{
		{"chat", req.ToChat, "tool_calls"},
		{"responses", req.ToResponses, "function_call"},
		{"anthropic", req.ToAnthropic, "tool_use"},
		{"gemini", req.ToGemini, "functionCall"},
	} {
		b, err := c.wire()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !strings.Contains(string(b), c.hasKey) {
			t.Errorf("%s wire misses %q: %s", c.name, c.hasKey, b)
		}
	}
	// Cross-protocol translation through Convert.
	chatBody, _ := req.ToChat()
	anthropic, err := llm.Convert(chatBody, llm.ProtocolChat, llm.ProtocolAnthropic)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(anthropic), `"tool_use"`) {
		t.Fatalf("Convert chat→anthropic misses tool_use: %s", anthropic)
	}
}
