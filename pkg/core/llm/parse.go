package llm

import (
	"encoding/json"
	"strings"

	"github.com/smallnest/matex/pkg/core/errs"
)

// The parseXRequest functions read a protocol's *request* body back into a
// canonical Request. They exist to back Convert, the cross-protocol
// translator: parse any of the four wire shapes, rebuild any other.

// Convert translates a request body from one protocol to another through the
// canonical Request. It is the package's cross-protocol converter: any of the
// four wire shapes in, any other wire shape out.
func Convert(body []byte, from, to Protocol) ([]byte, error) {
	af, ok := adapters[from]
	if !ok {
		return nil, errs.Invalid(0, "llm: unsupported protocol %q", from)
	}
	at, ok := adapters[to]
	if !ok {
		return nil, errs.Invalid(0, "llm: unsupported protocol %q", to)
	}
	r, err := af.parseRequest(body)
	if err != nil {
		return nil, errs.InternalWrap(0, err, "llm: parse %s request", from)
	}
	out, err := at.build(*r)
	if err != nil {
		return nil, errs.InternalWrap(0, err, "llm: build %s request", to)
	}
	return out, nil
}

// ---- OpenAI Chat Completions ---------------------------------------------------

func parseChatRequest(body []byte) (*Request, error) {
	var raw struct {
		Model    string `json:"model"`
		Messages []struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
		MaxTokens   int      `json:"max_tokens"`
		Temperature *float64 `json:"temperature"`
		TopP        *float64 `json:"top_p"`
		Stop        []string `json:"stop"`
		Tools       []struct {
			Function struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
		ToolChoice json.RawMessage `json:"tool_choice"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	r := &Request{Model: raw.Model, MaxTokens: raw.MaxTokens, Temperature: raw.Temperature, TopP: raw.TopP, Stop: raw.Stop}
	var systems []string
	for _, m := range raw.Messages {
		if m.Role == "system" {
			systems = append(systems, contentText(m.Content))
			continue
		}
		msg := Message{Role: m.Role, Content: contentText(m.Content), ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			msg.ToolCalls = append(msg.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
		}
		r.Messages = append(r.Messages, msg)
	}
	if len(systems) > 0 {
		r.System = strings.Join(systems, "\n")
	}
	for _, t := range raw.Tools {
		r.Tools = append(r.Tools, Tool{Name: t.Function.Name, Description: t.Function.Description, Schema: schemaObject(t.Function.Parameters)})
	}
	r.ToolChoice = toolChoiceFromWire(raw.ToolChoice)
	return r, nil
}

// ---- OpenAI Responses ------------------------------------------------------------

func parseResponsesRequest(body []byte) (*Request, error) {
	var raw struct {
		Model  string `json:"model"`
		System string `json:"instructions"`
		Input  []struct {
			Type      string          `json:"type"`
			Role      string          `json:"role"`
			CallID    string          `json:"call_id"`
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Output    string          `json:"output"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"input"`
		MaxTokens   int      `json:"max_output_tokens"`
		Temperature *float64 `json:"temperature"`
		TopP        *float64 `json:"top_p"`
		Tools       []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"tools"`
		ToolChoice json.RawMessage `json:"tool_choice"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	r := &Request{Model: raw.Model, System: raw.System, MaxTokens: raw.MaxTokens, Temperature: raw.Temperature, TopP: raw.TopP}
	for _, it := range raw.Input {
		switch it.Type {
		case "message":
			var text strings.Builder
			for _, c := range it.Content {
				if c.Type == "input_text" || c.Type == "output_text" || c.Type == "text" {
					text.WriteString(c.Text)
				}
			}
			r.Messages = append(r.Messages, Message{Role: it.Role, Content: text.String()})
		case "function_call":
			call := ToolCall{ID: it.CallID, Name: it.Name, Arguments: argsString(it.Arguments)}
			// Responses carries a call as its own item, beside the assistant
			// message that made it; reattach it so a round trip stays canonical.
			if n := len(r.Messages); n > 0 && r.Messages[n-1].Role == "assistant" {
				r.Messages[n-1].ToolCalls = append(r.Messages[n-1].ToolCalls, call)
			} else {
				r.Messages = append(r.Messages, Message{Role: "assistant", ToolCalls: []ToolCall{call}})
			}
		case "function_call_output":
			r.Messages = append(r.Messages, Message{Role: "tool", ToolCallID: it.CallID, Content: it.Output})
		}
	}
	for _, t := range raw.Tools {
		r.Tools = append(r.Tools, Tool{Name: t.Name, Description: t.Description, Schema: schemaObject(t.Parameters)})
	}
	r.ToolChoice = toolChoiceFromWire(raw.ToolChoice)
	return r, nil
}

// ---- Anthropic Messages ------------------------------------------------------------

func parseAnthropicRequest(body []byte) (*Request, error) {
	var raw struct {
		Model    string `json:"model"`
		System   string `json:"system"`
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string          `json:"type"`
				Text      string          `json:"text"`
				ID        string          `json:"id"`
				Name      string          `json:"name"`
				Input     json.RawMessage `json:"input"`
				ToolUseID string          `json:"tool_use_id"`
				Content   json.RawMessage `json:"content"`
			} `json:"content"`
		} `json:"messages"`
		MaxTokens   int      `json:"max_tokens"`
		Temperature *float64 `json:"temperature"`
		TopP        *float64 `json:"top_p"`
		Stop        []string `json:"stop_sequences"`
		Tools       []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
		ToolChoice json.RawMessage `json:"tool_choice"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	r := &Request{Model: raw.Model, System: raw.System, MaxTokens: raw.MaxTokens, Temperature: raw.Temperature, TopP: raw.TopP, Stop: raw.Stop}
	for _, m := range raw.Messages {
		if m.Role == "assistant" {
			msg := Message{Role: "assistant"}
			for _, b := range m.Content {
				switch b.Type {
				case "text":
					msg.Content += b.Text
				case "tool_use":
					msg.ToolCalls = append(msg.ToolCalls, ToolCall{ID: b.ID, Name: b.Name, Arguments: compactJSON(b.Input)})
				}
			}
			r.Messages = append(r.Messages, msg)
			continue
		}
		// A user message may hold tool results and its own text together;
		// split them back into tool and user messages.
		var texts []string
		for _, b := range m.Content {
			switch b.Type {
			case "text":
				texts = append(texts, b.Text)
			case "tool_result":
				r.Messages = append(r.Messages, Message{Role: "tool", ToolCallID: b.ToolUseID, Content: contentText(b.Content)})
			}
		}
		if len(texts) > 0 {
			r.Messages = append(r.Messages, Message{Role: "user", Content: strings.Join(texts, "\n")})
		}
	}
	for _, t := range raw.Tools {
		r.Tools = append(r.Tools, Tool{Name: t.Name, Description: t.Description, Schema: schemaObject(t.InputSchema)})
	}
	r.ToolChoice = toolChoiceFromWire(raw.ToolChoice)
	return r, nil
}

// ---- Google Gemini ------------------------------------------------------------------

func parseGeminiRequest(body []byte) (*Request, error) {
	var raw struct {
		SystemInstruction struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"systemInstruction"`
		Contents []struct {
			Role  string `json:"role"`
			Parts []struct {
				Text         string `json:"text"`
				FunctionCall struct {
					ID   string          `json:"id"`
					Name string          `json:"name"`
					Args json.RawMessage `json:"args"`
				} `json:"functionCall"`
				FunctionResponse struct {
					ID       string          `json:"id"`
					Name     string          `json:"name"`
					Response json.RawMessage `json:"response"`
				} `json:"functionResponse"`
			} `json:"parts"`
		} `json:"contents"`
		Tools []struct {
			FunctionDeclarations []struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"functionDeclarations"`
		} `json:"tools"`
		ToolConfig struct {
			FunctionCallingConfig struct {
				Mode                 string   `json:"mode"`
				AllowedFunctionNames []string `json:"allowedFunctionNames"`
			} `json:"functionCallingConfig"`
		} `json:"toolConfig"`
		GenerationConfig struct {
			Temperature     float64  `json:"temperature"`
			TopP            float64  `json:"topP"`
			MaxOutputTokens int      `json:"maxOutputTokens"`
			StopSequences   []string `json:"stopSequences"`
		} `json:"generationConfig"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	r := &Request{Model: "", MaxTokens: raw.GenerationConfig.MaxOutputTokens, Stop: raw.GenerationConfig.StopSequences}
	for _, p := range raw.SystemInstruction.Parts {
		r.System += p.Text
	}
	if raw.GenerationConfig.Temperature != 0 {
		t := raw.GenerationConfig.Temperature
		r.Temperature = &t
	}
	if raw.GenerationConfig.TopP != 0 {
		t := raw.GenerationConfig.TopP
		r.TopP = &t
	}
	for _, c := range raw.Contents {
		if c.Role == "model" {
			msg := Message{Role: "assistant"}
			for _, p := range c.Parts {
				msg.Content += p.Text
				if p.FunctionCall.Name != "" {
					msg.ToolCalls = append(msg.ToolCalls, ToolCall{ID: p.FunctionCall.ID, Name: p.FunctionCall.Name, Arguments: compactJSON(p.FunctionCall.Args)})
				}
			}
			r.Messages = append(r.Messages, msg)
			continue
		}
		for _, p := range c.Parts {
			if p.Text != "" {
				r.Messages = append(r.Messages, Message{Role: "user", Content: p.Text})
			}
			if p.FunctionResponse.Name != "" {
				r.Messages = append(r.Messages, Message{Role: "tool", Name: p.FunctionResponse.Name, Content: responseOutput(p.FunctionResponse.Response)})
			}
		}
	}
	for _, t := range raw.Tools {
		for _, d := range t.FunctionDeclarations {
			r.Tools = append(r.Tools, Tool{Name: d.Name, Description: d.Description, Schema: schemaObject(d.Parameters)})
		}
	}
	switch raw.ToolConfig.FunctionCallingConfig.Mode {
	case "NONE":
		r.ToolChoice = "none"
	case "ANY":
		if ns := raw.ToolConfig.FunctionCallingConfig.AllowedFunctionNames; len(ns) > 0 {
			r.ToolChoice = "name:" + ns[0]
		} else {
			r.ToolChoice = "required"
		}
	}
	return r, nil
}

// ---- shared wire helpers -------------------------------------------------------------

// contentText flattens a chat-style content value — a plain string or an array
// of typed parts — to its text.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "text" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}

// schemaObject decodes a wire tool schema into a plain value (a map), nil when
// the wire carried none. Builders marshal it back as-is.
func schemaObject(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	return v
}

// argsString normalizes a wire call's arguments — JSON string or object — to a
// JSON string.
func argsString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return compactJSON(raw)
}

// responseOutput extracts the text a Gemini functionResponse returned.
func responseOutput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var obj struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Output != "" {
		return obj.Output
	}
	return compactJSON(raw)
}

// toolChoiceFromWire reads a protocol's tool_choice value back into the
// canonical "auto" | "none" | "required" | "name:<tool>" form.
func toolChoiceFromWire(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "any":
			return "required"
		default:
			return s
		}
	}
	var obj struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		if obj.Function.Name != "" {
			return "name:" + obj.Function.Name
		}
		if obj.Name != "" {
			return "name:" + obj.Name
		}
	}
	return ""
}
