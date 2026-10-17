package llm

// System One is a question-answering API a Jev (decision) model serves: a
// free-form state and typed questions in, how likely every option was out, so
// a routing layer can tell what a message is and how hard a turn is to think
// about. The wire shape below is TypeSafe's System One; Vercel's AI Gateway
// and Cloudflare's Workers AI serve the same questions in their own envelope
// (JevViaVercelEval, JevViaCloudflare), normalized here to one canonical
// response.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/smallnest/matex/pkg/core/errs"
)

// Question is one typed question Jev answers. Type is "noul" (yes/no, how
// much yes as 0..1), "boolean" (Vercel's name for noul), "probability"
// (likelihoods over options), "score" (a level on the criteria's scale) or
// "choice" (pick one of the criteria).
type Question struct {
	Type         string
	Instructions string
	// Criteria are the options of a choice or score, each with what it is
	// for; "" leaves the option unnamed on the wire (null).
	Criteria map[string]string
}

// DecideRequest is one System One call.
type DecideRequest struct {
	Model string
	// State is the context Jev is asked about, e.g. {"message": text}.
	State map[string]any
	// Questions are asked by name; the reply answers them by the same names.
	Questions map[string]Question
}

// Answer is one answered question.
type Answer struct {
	Type string
	// Choice is the option picked for a choice, e.g. "bug fix".
	Choice string
	// Score is the level on the criteria's scale a score question rated.
	Score float64
	// Noul is how much yes, 0..1, a noul or boolean answered.
	Noul float64
	// Confidence is how sure Jev is of the likeliest option, 0..1.
	Confidence float64
	// Probabilities are each option's likelihood, when the question asked for
	// them.
	Probabilities map[string]float64
}

// DecideResponse is the System One reply.
type DecideResponse struct {
	Model   string
	Answers map[string]Answer
	Usage   Usage
}

// ToJev renders r as a System One request body. Criteria values are left as
// null on the wire when unnamed ("").
func (r DecideRequest) ToJev() ([]byte, error) {
	out := map[string]any{"model": r.Model}
	if len(r.State) > 0 {
		out["state"] = r.State
	}
	if len(r.Questions) > 0 {
		qs := make(map[string]any, len(r.Questions))
		for name, q := range r.Questions {
			m := map[string]any{"type": q.Type}
			if q.Instructions != "" {
				m["instructions"] = q.Instructions
			}
			if len(q.Criteria) > 0 {
				c := make(map[string]any, len(q.Criteria))
				for opt, what := range q.Criteria {
					if what == "" {
						c[opt] = nil
					} else {
						c[opt] = what
					}
				}
				m["criteria"] = c
			}
			qs[name] = m
		}
		out["questions"] = qs
	}
	return json.Marshal(out)
}

// ParseJev parses a System One reply.
func ParseJev(body []byte) (*DecideResponse, error) {
	var raw struct {
		Model   string `json:"model"`
		Answers map[string]struct {
			Type          string             `json:"type"`
			Choice        string             `json:"choice"`
			Score         float64            `json:"score"`
			Noul          float64            `json:"noul"`
			Confidence    float64            `json:"confidence"`
			Probabilities map[string]float64 `json:"probabilities"`
		} `json:"answers"`
		Usage struct {
			Input  int `json:"input_tokens"`
			Output int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	out := &DecideResponse{Model: raw.Model}
	out.Usage = Usage{InputTokens: raw.Usage.Input, OutputTokens: raw.Usage.Output, TotalTokens: raw.Usage.Input + raw.Usage.Output}
	for name, a := range raw.Answers {
		if out.Answers == nil {
			out.Answers = map[string]Answer{}
		}
		out.Answers[name] = Answer{
			Type: a.Type, Choice: a.Choice, Score: a.Score, Noul: a.Noul,
			Confidence: a.Confidence, Probabilities: a.Probabilities,
		}
	}
	return out, nil
}

// Decide asks the decision API at c's base URL the questions in req and
// returns the canonical reply. It speaks System One whatever c's protocol is
// (a provider may serve conversations and decisions both); the serving shape
// is c.jevVia (WithJevVia), System One by default. Model comes from req, else
// WithModel. No streaming: Jev answers in one call.
func (c *Client) Decide(ctx context.Context, req DecideRequest) (*DecideResponse, error) {
	if req.Model == "" {
		req.Model = c.model
	}
	if req.Model == "" {
		return nil, errs.Invalid(0, "llm: model is required (WithModel or DecideRequest.Model)")
	}
	body, err := req.ToJev()
	if err != nil {
		return nil, errs.InternalWrap(0, err, "llm: build decide request")
	}
	body = jevAsk(c.jevVia, req.Model, body)
	resp, err := c.post(ctx, jevPath(c.jevVia), jevHeaders(c.jevVia, req.Model, c.apiKey), body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, errs.Wrap(errs.KindUnavailable, 0, err, "llm: read decide reply")
	}
	respBody, err = jevAnswer(c.jevVia, respBody)
	if err != nil {
		return nil, errs.InternalWrap(0, err, "llm: parse decide reply")
	}
	out, err := ParseJev(respBody)
	if err != nil {
		return nil, errs.InternalWrap(0, err, "llm: parse decide reply")
	}
	return out, nil
}

// jevPath is where a question for a via is posted.
func jevPath(via JevVia) string {
	switch via {
	case JevViaVercelEval:
		return "/evaluation-model"
	case JevViaCloudflare:
		return "/ai/run"
	default:
		return "/systemone"
	}
}

// jevHeaders are the auth and, for Vercel's evaluation models, the headers
// that carry the model and the gateway protocol.
func jevHeaders(via JevVia, model, apiKey string) http.Header {
	h := http.Header{}
	if apiKey != "" {
		h.Set("Authorization", "Bearer "+apiKey)
	}
	if via == JevViaVercelEval {
		h.Set("ai-gateway-protocol-version", "0.0.1")
		h.Set("ai-gateway-auth-method", "api-key")
		h.Set("ai-model-id", model)
		h.Set("ai-evaluation-model-specification-version", "4")
	}
	return h
}

// jevAsk is a System One body (body) as via takes it: Vercel's evaluation
// models name the model in a header and ask a noul as a boolean; Workers AI
// takes the state and questions as the input of a run of the model.
func jevAsk(via JevVia, model string, body []byte) []byte {
	if via != JevViaVercelEval && via != JevViaCloudflare {
		return body
	}
	var q map[string]any
	if json.Unmarshal(body, &q) != nil {
		return body
	}
	delete(q, "model")
	switch via {
	case JevViaVercelEval:
		if qs, ok := q["questions"].(map[string]any); ok {
			for _, v := range qs {
				if x, ok := v.(map[string]any); ok && x["type"] == "noul" {
					x["type"] = "boolean"
				}
			}
		}
		b, _ := json.Marshal(q)
		return b
	case JevViaCloudflare:
		b, _ := json.Marshal(map[string]any{"model": model, "input": q})
		return b
	}
	return body
}

// jevAnswer is a gateway's answer as System One gives it: out of Cloudflare's
// result envelope, or, from Vercel's evaluation models, a boolean's
// probability as a noul and a confidence where the reply named none.
func jevAnswer(via JevVia, b []byte) ([]byte, error) {
	switch via {
	case JevViaCloudflare:
		var env struct {
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(b, &env); err != nil || len(env.Result) == 0 {
			return nil, errors.New("no result in Cloudflare answer")
		}
		return env.Result, nil
	case JevViaVercelEval:
		var v struct {
			Model   string                    `json:"model"`
			Answers map[string]map[string]any `json:"answers"`
			Usage   struct {
				Input  int `json:"inputTokens"`
				Output int `json:"outputTokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(b, &v); err != nil || v.Answers == nil {
			return b, nil // not the evaluation shape; pass it through
		}
		for _, a := range v.Answers {
			if a["type"] == "boolean" {
				p, _ := a["probability"].(float64)
				a["type"], a["noul"] = "noul", p
			}
			if _, ok := a["confidence"]; !ok {
				if ps, ok := a["probabilities"].(map[string]any); ok {
					a["confidence"] = jevConfidence(ps)
				}
			}
		}
		out, err := json.Marshal(map[string]any{
			"model": v.Model, "answers": v.Answers,
			"usage": map[string]int{"input_tokens": v.Usage.Input, "output_tokens": v.Usage.Output},
		})
		return out, err
	}
	return b, nil
}

// jevConfidence is how sure a set of probabilities is of its likeliest, as
// System One gives it: 0 with every option as likely, 1 with one certain.
func jevConfidence(ps map[string]any) float64 {
	top, n := 0.0, 0
	for _, v := range ps {
		if f, ok := v.(float64); ok {
			top, n = max(top, f), n+1
		}
	}
	if n < 2 {
		return 1
	}
	return max(0, (top-1/float64(n))/(1-1/float64(n)))
}
