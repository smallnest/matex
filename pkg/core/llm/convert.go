package llm

import (
	"encoding/json"
	"strings"
)

// The conversions render a canonical Request as each protocol's wire body and
// parse each protocol's reply back into the canonical Response. They are the
// package's public conversion API: use them to inspect the exact wire JSON a
// request becomes, or to translate between protocols yourself.

// ---- OpenAI Chat Completions ------------------------------------------------

// ToChat renders r as an OpenAI Chat Completions request body.
func (r Request) ToChat() ([]byte, error) {
	msgs := make([]any, 0, len(r.Messages)+1)
	if r.System != "" {
		msgs = append(msgs, chatMessage("system", r.System, "", nil))
	}
	for _, m := range r.Messages {
		switch m.Role {
		case "system", "tool":
			msgs = append(msgs, chatMessage(m.Role, m.Content, m.ToolCallID, nil))
		default: // user, assistant
			msgs = append(msgs, chatMessage(m.Role, m.Content, "", m.ToolCalls))
		}
	}
	out := map[string]any{"model": r.Model, "messages": msgs}
	if r.MaxTokens > 0 {
		out["max_tokens"] = r.MaxTokens
	}
	if r.Temperature != nil {
		out["temperature"] = *r.Temperature
	}
	if r.TopP != nil {
		out["top_p"] = *r.TopP
	}
	if len(r.Stop) > 0 {
		out["stop"] = r.Stop
	}
	if len(r.Tools) > 0 {
		tools := make([]any, 0, len(r.Tools))
		for _, t := range r.Tools {
			fn := map[string]any{"name": t.Name, "description": t.Description}
			if t.Schema != nil {
				fn["parameters"] = t.Schema
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		out["tools"] = tools
		if r.ToolChoice != "" {
			out["tool_choice"] = chatToolChoice(r.ToolChoice)
		}
	}
	if r.Stream {
		out["stream"] = true
		out["stream_options"] = map[string]any{"include_usage": true}
	}
	return json.Marshal(out)
}

func chatMessage(role, content, toolCallID string, calls []ToolCall) map[string]any {
	m := map[string]any{"role": role, "content": content}
	if toolCallID != "" {
		m["tool_call_id"] = toolCallID
	}
	if len(calls) > 0 {
		cc := make([]any, 0, len(calls))
		for _, tc := range calls {
			cc = append(cc, map[string]any{
				"id": tc.ID, "type": "function",
				"function": map[string]any{"name": tc.Name, "arguments": tc.Arguments},
			})
		}
		m["tool_calls"] = cc
	}
	return m
}

func chatToolChoice(tc string) any {
	if name, ok := strings.CutPrefix(tc, "name:"); ok {
		return map[string]any{"type": "function", "function": map[string]any{"name": name}}
	}
	return tc
}

// ParseChat parses an OpenAI Chat Completions reply.
func ParseChat(body []byte) (*Response, error) {
	var raw struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	out := &Response{ID: raw.ID, Model: raw.Model}
	out.Usage = Usage{InputTokens: raw.Usage.PromptTokens, OutputTokens: raw.Usage.CompletionTokens, TotalTokens: raw.Usage.TotalTokens}
	if len(raw.Choices) > 0 {
		m := raw.Choices[0].Message
		out.Content = m.Content
		out.FinishReason = chatFinish(raw.Choices[0].FinishReason)
		for _, tc := range m.ToolCalls {
			out.ToolCalls = append(out.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
		}
	}
	return out, nil
}

func chatFinish(s string) string {
	switch s {
	case "stop":
		return "stop"
	case "length":
		return "length"
	case "tool_calls", "function_call":
		return "tool_calls"
	case "content_filter":
		return "content_filter"
	case "":
		return ""
	default:
		return "other"
	}
}

// ---- OpenAI Responses --------------------------------------------------------

// ToResponses renders r as an OpenAI Responses request body.
func (r Request) ToResponses() ([]byte, error) {
	input := make([]any, 0, len(r.Messages))
	for _, m := range r.Messages {
		switch m.Role {
		case "system":
			input = append(input, responseMessage("system", "input_text", m.Content))
		case "tool":
			input = append(input, map[string]any{"type": "function_call_output", "call_id": m.ToolCallID, "output": m.Content})
		case "assistant":
			if m.Content != "" {
				input = append(input, responseMessage("assistant", "output_text", m.Content))
			}
			for _, tc := range m.ToolCalls {
				input = append(input, map[string]any{"type": "function_call", "call_id": tc.ID, "name": tc.Name, "arguments": tc.Arguments})
			}
		default: // user
			if m.Content != "" {
				input = append(input, responseMessage("user", "input_text", m.Content))
			}
		}
	}
	if input == nil {
		input = []any{}
	}
	out := map[string]any{"model": r.Model, "input": input, "store": false}
	if r.System != "" {
		out["instructions"] = r.System
	}
	if r.MaxTokens > 0 {
		out["max_output_tokens"] = r.MaxTokens
	}
	if r.Temperature != nil {
		out["temperature"] = *r.Temperature
	}
	if r.TopP != nil {
		out["top_p"] = *r.TopP
	}
	if len(r.Tools) > 0 {
		tools := make([]any, 0, len(r.Tools))
		for _, t := range r.Tools {
			tool := map[string]any{"type": "function", "name": t.Name, "description": t.Description}
			if t.Schema != nil {
				tool["parameters"] = t.Schema
			}
			tools = append(tools, tool)
		}
		out["tools"] = tools
		if r.ToolChoice != "" {
			out["tool_choice"] = responsesToolChoice(r.ToolChoice)
		}
	}
	if r.Stream {
		out["stream"] = true
	}
	return json.Marshal(out)
}

func responseMessage(role, textType, text string) map[string]any {
	return map[string]any{
		"type": "message", "role": role,
		"content": []any{map[string]any{"type": textType, "text": text}},
	}
}

func responsesToolChoice(tc string) any {
	if name, ok := strings.CutPrefix(tc, "name:"); ok {
		return map[string]any{"type": "function", "name": name}
	}
	return tc
}

// ParseResponses parses an OpenAI Responses reply.
func ParseResponses(body []byte) (*Response, error) {
	var raw struct {
		ID     string `json:"id"`
		Model  string `json:"model"`
		Status string `json:"status"`
		Output []struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		IncompleteDetails struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	out := &Response{ID: raw.ID, Model: raw.Model}
	out.Usage = Usage{InputTokens: raw.Usage.InputTokens, OutputTokens: raw.Usage.OutputTokens, TotalTokens: raw.Usage.TotalTokens}
	for _, it := range raw.Output {
		switch it.Type {
		case "message":
			for _, c := range it.Content {
				if c.Type == "output_text" {
					out.Content += c.Text
				}
			}
		case "function_call":
			out.ToolCalls = append(out.ToolCalls, ToolCall{ID: it.ID, Name: it.Name, Arguments: it.Arguments})
		}
	}
	out.FinishReason = responsesFinish(raw.Status, raw.IncompleteDetails.Reason, len(out.ToolCalls) > 0)
	return out, nil
}

func responsesFinish(status, incomplete string, hasCalls bool) string {
	switch {
	case hasCalls:
		return "tool_calls"
	case status == "completed":
		return "stop"
	case status == "incomplete":
		switch incomplete {
		case "max_output_tokens":
			return "length"
		case "content_filter":
			return "content_filter"
		default:
			return "other"
		}
	case status == "":
		return ""
	default: // failed, cancelled, …
		return "other"
	}
}

// ---- Anthropic Messages ------------------------------------------------------

// ToAnthropic renders r as an Anthropic Messages request body. The API
// requires max_tokens, defaulted to defaultAnthropicMaxTokens when unset.
func (r Request) ToAnthropic() ([]byte, error) {
	maxTokens := r.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultAnthropicMaxTokens
	}
	out := map[string]any{"model": r.Model, "max_tokens": maxTokens}
	if r.System != "" {
		out["system"] = r.System
	}
	if msgs := anthropicMessages(r.Messages); len(msgs) > 0 {
		out["messages"] = msgs
	}
	// Anthropic takes temperature or top_p, not both; temperature wins.
	if r.Temperature != nil {
		out["temperature"] = *r.Temperature
	} else if r.TopP != nil {
		out["top_p"] = *r.TopP
	}
	if len(r.Stop) > 0 {
		out["stop_sequences"] = r.Stop
	}
	if len(r.Tools) > 0 {
		tools := make([]any, 0, len(r.Tools))
		for _, t := range r.Tools {
			// input_schema is required by the API; default to a bare object.
			schema := t.Schema
			if schema == nil {
				schema = map[string]any{"type": "object"}
			}
			tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "input_schema": schema})
		}
		out["tools"] = tools
		if r.ToolChoice != "" {
			out["tool_choice"] = anthropicToolChoice(r.ToolChoice)
		}
	}
	if r.Stream {
		out["stream"] = true
	}
	return json.Marshal(out)
}

// anthropicMessages reshapes canonical messages into Anthropic's: user and
// assistant alternate (consecutive ones merge), tool results live inside a
// user message, and an assistant's calls become tool_use blocks in one message.
func anthropicMessages(msgs []Message) []any {
	var out []any
	var userBlocks []any
	flushUser := func() {
		if len(userBlocks) > 0 {
			out = append(out, map[string]any{"role": "user", "content": userBlocks})
			userBlocks = nil
		}
	}
	for _, m := range msgs {
		switch m.Role {
		case "system":
			if m.Content != "" { // no top-level slot here; fold into the next user turn
				userBlocks = append(userBlocks, map[string]any{"type": "text", "text": m.Content})
			}
		case "assistant":
			flushUser()
			blocks := make([]any, 0, 1+len(m.ToolCalls))
			if m.Content != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": m.Content})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Name, "input": rawObject(tc.Arguments)})
			}
			if len(blocks) > 0 {
				if n := len(out); n > 0 && out[n-1].(map[string]any)["role"] == "assistant" {
					prev := out[n-1].(map[string]any)
					prev["content"] = append(prev["content"].([]any), blocks...)
				} else {
					out = append(out, map[string]any{"role": "assistant", "content": blocks})
				}
			}
		case "tool":
			userBlocks = append(userBlocks, map[string]any{"type": "tool_result", "tool_use_id": m.ToolCallID, "content": m.Content})
		default: // user
			if m.Content != "" {
				userBlocks = append(userBlocks, map[string]any{"type": "text", "text": m.Content})
			}
		}
	}
	flushUser()
	return out
}

func anthropicToolChoice(tc string) any {
	if name, ok := strings.CutPrefix(tc, "name:"); ok {
		return map[string]any{"type": "tool", "name": name}
	}
	switch tc {
	case "required":
		return "any"
	default:
		return tc
	}
}

// ParseAnthropic parses an Anthropic Messages reply.
func ParseAnthropic(body []byte) (*Response, error) {
	var raw struct {
		ID         string `json:"id"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	out := &Response{ID: raw.ID, Model: raw.Model, FinishReason: anthropicFinish(raw.StopReason)}
	out.Usage = Usage{InputTokens: raw.Usage.InputTokens, OutputTokens: raw.Usage.OutputTokens, TotalTokens: raw.Usage.InputTokens + raw.Usage.OutputTokens}
	for _, b := range raw.Content {
		switch b.Type {
		case "text":
			out.Content += b.Text
		case "tool_use":
			out.ToolCalls = append(out.ToolCalls, ToolCall{ID: b.ID, Name: b.Name, Arguments: compactJSON(b.Input)})
		}
	}
	return out, nil
}

func anthropicFinish(s string) string {
	switch s {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "refusal"
	case "":
		return ""
	default:
		return "other"
	}
}

// ---- Google Gemini ------------------------------------------------------------

// ToGemini renders r as a Gemini generateContent request body.
func (r Request) ToGemini() ([]byte, error) {
	out := map[string]any{"contents": geminiContents(r.Messages)}
	if r.System != "" {
		out["systemInstruction"] = map[string]any{"parts": []any{map[string]any{"text": r.System}}}
	}
	if len(r.Tools) > 0 {
		decls := make([]any, 0, len(r.Tools))
		for _, t := range r.Tools {
			d := map[string]any{"name": t.Name, "description": t.Description}
			if t.Schema != nil {
				d["parameters"] = t.Schema
			}
			decls = append(decls, d)
		}
		out["tools"] = []any{map[string]any{"functionDeclarations": decls}}
	}
	if r.ToolChoice != "" {
		out["toolConfig"] = map[string]any{"functionCallingConfig": geminiToolConfig(r.ToolChoice)}
	}
	gen := map[string]any{}
	if r.Temperature != nil {
		gen["temperature"] = *r.Temperature
	}
	if r.TopP != nil {
		gen["topP"] = *r.TopP
	}
	if r.MaxTokens > 0 {
		gen["maxOutputTokens"] = r.MaxTokens
	}
	if len(r.Stop) > 0 {
		gen["stopSequences"] = r.Stop
	}
	if len(gen) > 0 {
		out["generationConfig"] = gen
	}
	return json.Marshal(out)
}

// geminiContents reshapes canonical messages into Gemini contents. Roles are
// user and model; tool results become functionResponse parts, calls become
// functionCall parts, consecutive same-role contents merge. Gemini's
// functionResponse needs the called function's name: it is looked up from the
// assistant's calls by ToolCallID, falling back to Message.Name, then "tool".
func geminiContents(msgs []Message) []any {
	callNames := make(map[string]string, len(msgs))
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			callNames[tc.ID] = tc.Name
		}
	}
	var out []any
	add := func(role string, parts []any) {
		if len(parts) == 0 {
			return
		}
		if n := len(out); n > 0 && out[n-1].(map[string]any)["role"] == role {
			prev := out[n-1].(map[string]any)
			prev["parts"] = append(prev["parts"].([]any), parts...)
			return
		}
		out = append(out, map[string]any{"role": role, "parts": parts})
	}
	for _, m := range msgs {
		switch m.Role {
		case "system":
			if m.Content != "" { // no top-level slot here; fold into the next user turn
				add("user", []any{map[string]any{"text": m.Content}})
			}
		case "assistant":
			parts := make([]any, 0, 1+len(m.ToolCalls))
			if m.Content != "" {
				parts = append(parts, map[string]any{"text": m.Content})
			}
			for _, tc := range m.ToolCalls {
				parts = append(parts, map[string]any{"functionCall": map[string]any{"id": tc.ID, "name": tc.Name, "args": rawObject(tc.Arguments)}})
			}
			add("model", parts)
		case "tool":
			name := callNames[m.ToolCallID]
			if name == "" {
				name = m.Name
			}
			if name == "" {
				name = "tool"
			}
			add("user", []any{map[string]any{"functionResponse": map[string]any{"name": name, "response": map[string]any{"output": m.Content}}}})
		default: // user
			if m.Content != "" {
				add("user", []any{map[string]any{"text": m.Content}})
			}
		}
	}
	return out
}

func geminiToolConfig(tc string) map[string]any {
	if name, ok := strings.CutPrefix(tc, "name:"); ok {
		return map[string]any{"mode": "ANY", "allowedFunctionNames": []string{name}}
	}
	switch tc {
	case "none":
		return map[string]any{"mode": "NONE"}
	case "required":
		return map[string]any{"mode": "ANY"}
	default: // auto, ""
		return map[string]any{"mode": "AUTO"}
	}
}

// ParseGemini parses a Gemini generateContent reply.
func ParseGemini(body []byte) (*Response, error) {
	var raw struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text         string `json:"text"`
					FunctionCall struct {
						Name string          `json:"name"`
						Args json.RawMessage `json:"args"`
					} `json:"functionCall"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
			TotalTokenCount      int `json:"totalTokenCount"`
		} `json:"usageMetadata"`
		ModelVersion string `json:"modelVersion"`
		ResponseID   string `json:"responseId"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	out := &Response{ID: raw.ResponseID, Model: raw.ModelVersion}
	out.Usage = Usage{InputTokens: raw.UsageMetadata.PromptTokenCount, OutputTokens: raw.UsageMetadata.CandidatesTokenCount, TotalTokens: raw.UsageMetadata.TotalTokenCount}
	if len(raw.Candidates) > 0 {
		c := raw.Candidates[0]
		for _, p := range c.Content.Parts {
			if p.Text != "" {
				out.Content += p.Text
			}
			if p.FunctionCall.Name != "" {
				out.ToolCalls = append(out.ToolCalls, ToolCall{Name: p.FunctionCall.Name, Arguments: compactJSON(p.FunctionCall.Args)})
			}
		}
		out.FinishReason = geminiFinish(c.FinishReason)
	}
	return out, nil
}

func geminiFinish(s string) string {
	switch s {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "BLOCKLIST":
		return "content_filter"
	case "":
		return ""
	default:
		return "other"
	}
}
