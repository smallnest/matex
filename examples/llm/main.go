// Command llm shows the unified LLM client (pkg/core/llm).
//
// One canonical request is rendered as all four wire protocols — OpenAI Chat
// Completions, OpenAI Responses, Anthropic Messages, Google Gemini — then sent
// to a local fake upstream (httptest) over the Chat protocol, both
// non-streaming and streaming; and a Jev decision is asked over the System One
// protocol at a fake System One upstream. Nothing leaves the machine and no
// API key is needed.
//
// Run: go run ./examples/llm
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"time"

	"github.com/smallnest/matex/pkg/core/llm"
)

func main() {
	req := demoRequest()

	// 1. 四种协议的转换：同一份请求 → 各协议的 wire body。
	fmt.Println("== 一份 canonical 请求 → 四种协议 ==")
	printWire("OpenAI Chat Completions", req.ToChat)
	printWire("OpenAI Responses", req.ToResponses)
	printWire("Anthropic Messages", req.ToAnthropic)
	printWire("Google Gemini", req.ToGemini)

	// 协议互转：Chat 的 wire body → Anthropic 的 wire body。
	chatBody, _ := req.ToChat()
	anthropic, err := llm.Convert(chatBody, llm.ProtocolChat, llm.ProtocolAnthropic)
	if err != nil {
		fatalf("Convert: %v", err)
	}
	fmt.Println("== Convert: Chat wire → Anthropic wire ==")
	fmt.Println(indent(anthropic))

	// 2. 统一访问：指向本地假上游，离线跑通 Chat 协议。
	srv := newFakeChatServer()
	defer srv.Close()

	c := llm.New(
		llm.WithBaseURL(srv.URL),
		llm.WithAPIKey("test"),
		llm.WithModel("gpt-4o-mini"),
		llm.WithTimeout(10*time.Second),
		llm.WithMaxTokens(256),
	)

	fmt.Println("== Chat（非流式）==")
	resp, err := c.Chat(context.Background(), req)
	if err != nil {
		fatalf("Chat: %v", err)
	}
	fmt.Printf("id=%s model=%s finish=%s content=%q\n", resp.ID, resp.Model, resp.FinishReason, resp.Content)
	fmt.Printf("usage in=%d out=%d total=%d\n", resp.Usage.InputTokens, resp.Usage.OutputTokens, resp.Usage.TotalTokens)

	fmt.Println("== ChatStream（流式）==")
	ch, err := c.ChatStream(context.Background(), req)
	if err != nil {
		fatalf("ChatStream: %v", err)
	}
	for ev := range ch {
		switch ev.Kind {
		case llm.EventText:
			fmt.Print(ev.Text)
		case llm.EventToolCall:
			fmt.Printf("\n[tool call] %s(%s)\n", ev.ToolCall.Name, ev.ToolCall.Arguments)
		case llm.EventUsage:
			fmt.Printf("\n[usage] in=%d out=%d total=%d\n", ev.Usage.InputTokens, ev.Usage.OutputTokens, ev.Usage.TotalTokens)
		case llm.EventDone:
			fmt.Printf("[done] finish=%s\n", ev.Stop)
		case llm.EventError:
			fatalf("stream: %v", ev.Err)
		}
	}

	// 3. Jev 决策问答：System One 协议，带类型的问题（choice / score）。
	jevsrv := newFakeJevServer()
	defer jevsrv.Close()

	jc := llm.New(
		llm.WithBaseURL(jevsrv.URL),
		llm.WithAPIKey("test"),
		llm.WithModel("jev-latest"),
		llm.WithTimeout(10*time.Second),
	)
	fmt.Println("== Jev Decide（System One 决策问答）==")
	dr, err := jc.Decide(context.Background(), llm.DecideRequest{
		State: map[string]any{"message": "这个查询接口偶尔超时，帮我修一下。"},
		Questions: map[string]llm.Question{
			"intent": {Type: "choice", Instructions: "Which kind of request fits it best?",
				Criteria: map[string]string{"greeting": "", "bug fix": "an ordinary fix", "feature": "a small feature"}},
			"effort": {Type: "score", Instructions: "How much reasoning does a coding assistant need?",
				Bands: []string{"a one-line change", "an ordinary fix", "a change across files"}},
		},
	})
	if err != nil {
		fatalf("Decide: %v", err)
	}
	fmt.Printf("intent: %s (confidence %.2f)\n", dr.Answers["intent"].Choice, dr.Answers["intent"].Confidence)
	fmt.Printf("effort: score %.0f\n", dr.Answers["effort"].Score)
	fmt.Printf("usage in=%d out=%d total=%d\n", dr.Usage.InputTokens, dr.Usage.OutputTokens, dr.Usage.TotalTokens)
}

// demoRequest is a canonical request exercising text, a tool call and a tool
// result — the shapes every protocol has to carry.
func demoRequest() llm.Request {
	return llm.Request{
		Model:      "demo-model",
		System:     "你是简洁的中文助手。",
		MaxTokens:  128,
		ToolChoice: "auto",
		Messages: []llm.Message{
			{Role: "user", Content: "北京今天天气如何？"},
			{Role: "assistant", Content: "", ToolCalls: []llm.ToolCall{
				{ID: "call_1", Name: "get_weather", Arguments: `{"city":"beijing"}`},
			}},
			{Role: "tool", ToolCallID: "call_1", Name: "get_weather", Content: `{"weather":"sunny","temp":21}`},
			{Role: "user", Content: "适合散步吗？"},
		},
		Tools: []llm.Tool{{
			Name:        "get_weather",
			Description: "查询城市天气",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"city": map[string]any{"type": "string"},
				},
				"required": []string{"city"},
			},
		}},
	}
}

// newFakeJevServer is an in-process upstream speaking the System One API: it
// answers a /systemone call with a canned intent (choice) and effort (score)
// verdict.
func newFakeJevServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/systemone" {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test" {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"model":"jev-latest","answers":{
			"intent":{"type":"choice","choice":"bug fix","confidence":0.9,"probabilities":{"greeting":0.02,"bug fix":0.9,"feature":0.08}},
			"effort":{"type":"score","score":1.0}
		},"usage":{"input_tokens":40,"output_tokens":12}}`)
	}))
}

// newFakeChatServer is an in-process upstream speaking the OpenAI Chat
// protocol: it answers non-streaming calls with one completion and streaming
// calls with SSE text.
func newFakeChatServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test" {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		var body struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: "+`{"choices":[{"delta":{"content":"北京今天"}}]}`+"\n\n")
			_, _ = fmt.Fprint(w, "data: "+`{"choices":[{"delta":{"content":"晴，21°C，"}}]}`+"\n\n")
			_, _ = fmt.Fprint(w, "data: "+`{"choices":[{"delta":{"content":"适合散步。"}}]}`+"\n\n")
			_, _ = fmt.Fprint(w, "data: "+`{"choices":[{"delta":{},"finish_reason":"stop"}]}`+"\n\n")
			_, _ = fmt.Fprint(w, "data: "+`{"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":9,"total_tokens":21}}`+"\n\n")
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-1","model":"demo-model","choices":[{"message":{"role":"assistant","content":"北京今天晴，21°C，适合散步。"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":9,"total_tokens":21}}`)
	}))
}

func printWire(name string, f func() ([]byte, error)) {
	b, err := f()
	if err != nil {
		fatalf("%s: %v", name, err)
	}
	fmt.Printf("-- %s --\n%s\n", name, indent(b))
}

func indent(b []byte) string {
	var out bytes.Buffer
	_ = json.Indent(&out, b, "", "  ")
	return out.String()
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
