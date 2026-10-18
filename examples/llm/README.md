# llm — 统一 LLM 客户端

`pkg/core/llm` 是 matex 的 LLM 客户端：一份**统一的请求/响应模型**（canonical
`Request` / `Response`），在四种 wire 协议间互转，并提供统一的访问入口；另外
用 `Client.Decide` 走 **Jev（System One）决策问答**。

| 协议 | 常量 | 端点 |
|---|---|---|
| OpenAI Chat Completions | `llm.ProtocolChat` | `{base}/chat/completions` |
| OpenAI Responses | `llm.ProtocolResponses` | `{base}/responses` |
| Anthropic Messages | `llm.ProtocolAnthropic` | `{base}/v1/messages` |
| Google Gemini generateContent | `llm.ProtocolGemini` | `{base}/models/{model}:generateContent` |
| Jev / System One（决策） | `llm.ProtocolJev` | 见下文「Jev 决策问答」 |

## 运行

```sh
go run ./examples/llm
go test ./examples/llm
```

示例先打印同一份请求翻译成的四种 wire body，再用进程内假上游（httptest，离线）
跑通 Chat 协议的非流式与流式调用，最后在假 System One 上游上问一次 Jev。

## 用法

```go
c := llm.New(
	llm.WithBaseURL("https://api.openai.com/v1"), // 按域名自动推断协议
	llm.WithAPIKey(os.Getenv("OPENAI_API_KEY")),
	llm.WithModel("gpt-4o-mini"),   // 请求没写 model 时用它
	llm.WithTimeout(30*time.Second), // 默认 60s
	llm.WithMaxTokens(1024),        // 请求没写 max_tokens 时用它
	// llm.WithProtocol(llm.ProtocolAnthropic) // 强制协议，缺省按 base_url 推断
)

resp, err := c.Chat(ctx, llm.Request{Messages: []llm.Message{{Role: "user", Content: "hi"}}})
// resp.Content / resp.ToolCalls / resp.FinishReason / resp.Usage

// 流式：统一事件
ch, err := c.ChatStream(ctx, req)
defer llm.StopStream(ch) // 提前退出也能释放连接与 goroutine
for ev := range ch { /* EventText / EventToolCall / EventUsage / EventDone / EventError */ }
```

超时语义：`WithTimeout`（默认 60s）约束整次非流式调用；流式调用只用它约束
**等到响应头**（time-to-first-byte），响应体开始流动后由 `ctx` 决定何时结束——
慢但持续的流不会被从中间截断，调用方仍可用自己的 deadline 限时。

## 四种协议的转换

- `Request.ToChat() / ToResponses() / ToAnthropic() / ToGemini()`：
  一份 canonical 请求 → 各协议 wire body。
- `ParseChat / ParseResponses / ParseAnthropic / ParseGemini`：各协议响应 → canonical `Response`。
- `llm.Convert(body, from, to)`：任意协议的请求 body → 任意另一协议，走 canonical 中转。

## Jev 决策问答（System One）

Jev 不回答对话，而是回答**带类型的问题**：`noul`（是非，0..1 的“是”的程度）、
`score`（按 criteria 打分）、`choice`（从 criteria 里选一项）、`probability`
（各选项概率）。它给每个选项一个把握度（`confidence`），供路由层判定一条消息
属于哪个 intent、这一轮要多重推理。

```go
jc := llm.New(
	llm.WithBaseURL("https://api.typesafe.ai/v1"), // 域名含 typesafe 自动推断 jev
	llm.WithAPIKey(os.Getenv("TYPESAFE_API_KEY")),
	llm.WithModel("jev-latest"),
	// llm.WithJevVia(llm.JevViaSystemOne), // 默认；另有 vercel-eval / cloudflare
)

out, err := jc.Decide(ctx, llm.DecideRequest{
	State:     map[string]any{"message": "这个查询接口偶尔超时，帮我修一下。"},
	Questions: map[string]llm.Question{
		"intent": {Type: "choice", Instructions: "Which kind of request fits it best?",
			Criteria: map[string]string{"greeting": "", "bug fix": "an ordinary fix"}},
		"effort": {Type: "score", Instructions: "How much reasoning?",
			Bands: []string{"a one-line change", "an ordinary fix", "a change across files"}},
	},
})
// out.Answers["intent"].Choice / .Confidence / .Probabilities
// out.Answers["effort"].Score ; out.Usage
```

三种托管形态（`WithJevVia`）：

| via | 端点 | 差异 |
|---|---|---|
| `JevViaSystemOne`（默认） | `{base}/systemone` | body 原样（TypeSafe、magpie 网关） |
| `JevViaVercelEval` | `{base}/evaluation-model` | model 走 `ai-model-id` 头，noul 以 boolean 问；应答归一为 noul，缺 `confidence` 时由概率算出 |
| `JevViaCloudflare` | `{base}/ai/run`（base 是账户 URL，如 `.../accounts/<id>`，不含 `/ai`） | body 包成 `{model,input}`，answer 从 `result` 解封 |

协议互转（`Convert`）只覆盖四种对话协议；Jev 有自己的 canonical 类型
（`DecideRequest` / `DecideResponse`），不进转换集。

## 关键点

- **工具调用**：`Message{Role:"tool"}` 携带 `ToolCallID`，assistant 消息携带
  `ToolCalls`；各协议自动互转（Anthropic/Gemini 的 arguments 是对象，Chat/Responses 是字符串）。
- **Anthropic**：`max_tokens` 必填，未指定默认 4096；连续 user 消息自动合并
  （tool_result 并入 user 消息）。
- **Gemini**：role 为 `user`/`model`；`functionResponse` 需要函数名，自动从
  之前的 assistant 调用按 `ToolCallID` 解析，兜底取 `Message.Name`。
- **中断**：`StopStream(ch)` 排空并结束流；不消费的流不会永久占用连接。
- **错误**：统一映射到 `errs`（400→Invalid、401→Unauthorized、429→RateLimited、
  5xx→Unavailable、超时→Timeout），上游错误体节选进消息。

## 约定

只经 `pkg/core/llm` 访问 LLM，不直接 import 各家 SDK（depguard 之外的业务纪律）；
包内仅用 `net/http` + `encoding/json`，不新增第三方依赖。
