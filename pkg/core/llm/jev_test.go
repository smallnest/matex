package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/smallnest/matex/pkg/core/errs"
)

func TestToJev(t *testing.T) {
	r := DecideRequest{
		Model: "jev-latest",
		State: map[string]any{"message": "这个接口偶尔超时"},
		Questions: map[string]Question{
			"intent": {Type: "choice", Instructions: "Which kind of request fits it best?",
				Criteria: map[string]string{"greeting": "", "bug fix": "an ordinary fix"}},
			"effort": {Type: "score", Instructions: "How much reasoning?",
				Criteria: map[string]string{"low": "a one-line change", "high": ""}},
		},
	}
	m := decode(t, conv(t, r.ToJev))
	if m["model"] != "jev-latest" {
		t.Fatalf("model = %v", m["model"])
	}
	if m["state"].(map[string]any)["message"] != "这个接口偶尔超时" {
		t.Fatalf("state = %v", m["state"])
	}
	qs := m["questions"].(map[string]any)
	intent := qs["intent"].(map[string]any)
	if intent["type"] != "choice" || intent["instructions"] == "" {
		t.Fatalf("intent = %v", intent)
	}
	crit := intent["criteria"].(map[string]any)
	if crit["greeting"] != nil || crit["bug fix"] != "an ordinary fix" {
		t.Fatalf("intent criteria = %v (unnamed option must be null)", crit)
	}
	effort := qs["effort"].(map[string]any)
	if effort["type"] != "score" || effort["criteria"].(map[string]any)["high"] != nil {
		t.Fatalf("effort = %v", effort)
	}
}

func TestParseJev(t *testing.T) {
	body := []byte(`{"model":"jev-latest","answers":{
		"ok":{"type":"noul","noul":0.85},
		"intent":{"type":"choice","choice":"bug fix","confidence":0.9,"probabilities":{"greeting":0.02,"bug fix":0.9}},
		"effort":{"type":"score","score":1.0}
	},"usage":{"input_tokens":40,"output_tokens":12}}`)
	out, err := ParseJev(body)
	if err != nil {
		t.Fatal(err)
	}
	if out.Model != "jev-latest" {
		t.Fatalf("model = %q", out.Model)
	}
	if out.Answers["ok"].Noul != 0.85 {
		t.Fatalf("ok = %+v", out.Answers["ok"])
	}
	i := out.Answers["intent"]
	if i.Type != "choice" || i.Choice != "bug fix" || i.Confidence != 0.9 || i.Probabilities["bug fix"] != 0.9 {
		t.Fatalf("intent = %+v", i)
	}
	if out.Answers["effort"].Score != 1.0 {
		t.Fatalf("effort = %+v", out.Answers["effort"])
	}
	if out.Usage != (Usage{InputTokens: 40, OutputTokens: 12, TotalTokens: 52}) {
		t.Fatalf("usage = %+v", out.Usage)
	}
}

func TestJevHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/systemone" {
			http.Error(w, "wrong path "+r.URL.Path, http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test" {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		var body struct {
			Model     string          `json:"model"`
			Questions json.RawMessage `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != "jev-latest" || len(body.Questions) == 0 {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// deliberate non-deterministic map order: answers keyed by name must survive
		_, _ = w.Write([]byte(`{"model":"jev-latest","answers":{"effort":{"type":"score","score":2.0},"intent":{"type":"choice","choice":"bug fix","confidence":0.8}},"usage":{"input_tokens":40,"output_tokens":12}}`))
	}))
	defer srv.Close()

	c := New(WithBaseURL(srv.URL), WithAPIKey("test"), WithModel("jev-latest"), WithTimeout(5*time.Second))
	out, err := c.Decide(context.Background(), DecideRequest{
		State: map[string]any{"message": "hi"},
		Questions: map[string]Question{
			"intent": {Type: "choice", Criteria: map[string]string{"bug fix": ""}},
			"effort": {Type: "score", Criteria: map[string]string{"low": ""}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Answers["intent"].Choice != "bug fix" || out.Answers["effort"].Score != 2.0 {
		t.Fatalf("answers = %+v", out.Answers)
	}
	if out.Usage.TotalTokens != 52 {
		t.Fatalf("usage = %+v", out.Usage)
	}
}

func TestJevVercelEval(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/evaluation-model" {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		if r.Header.Get("ai-model-id") != "jev-latest" {
			http.Error(w, "missing ai-model-id", http.StatusBadRequest)
			return
		}
		if r.Header.Get("ai-gateway-protocol-version") != "0.0.1" {
			http.Error(w, "missing gateway version", http.StatusBadRequest)
			return
		}
		var body struct {
			Model     string         `json:"model"`
			Questions map[string]any `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != "" {
			http.Error(w, "model must ride in the header", http.StatusBadRequest)
			return
		}
		if tpe := body.Questions["ok"].(map[string]any)["type"]; tpe != "boolean" {
			http.Error(w, "noul must be asked as boolean", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"jev-latest","answers":{"ok":{"type":"boolean","probability":0.7,"probabilities":{"yes":0.75,"no":0.25}}},"usage":{"inputTokens":20,"outputTokens":5}}`))
	}))
	defer srv.Close()

	c := New(WithBaseURL(srv.URL), WithAPIKey("test"), WithModel("jev-latest"),
		WithJevVia(JevViaVercelEval), WithTimeout(5*time.Second))
	out, err := c.Decide(context.Background(), DecideRequest{
		Questions: map[string]Question{"ok": {Type: "noul", Instructions: "Is this a test?"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	a := out.Answers["ok"]
	if a.Type != "noul" || a.Noul != 0.7 {
		t.Fatalf("ok = %+v, want the boolean normalized back to a noul", a)
	}
	// (0.75 - 0.5) / (1 - 0.5) = 0.5
	if a.Confidence != 0.5 {
		t.Fatalf("confidence = %v, want 0.5 from the probabilities", a.Confidence)
	}
	if out.Usage.InputTokens != 20 || out.Usage.OutputTokens != 5 {
		t.Fatalf("usage = %+v", out.Usage)
	}
}

func TestJevCloudflare(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ai/run" {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		var env struct {
			Model string          `json:"model"`
			Input json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			http.Error(w, "bad envelope", http.StatusBadRequest)
			return
		}
		if env.Model != "typesafe/jev" || !strings.Contains(string(env.Input), `"questions"`) {
			http.Error(w, "request must ride in {model, input}", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"model":"typesafe/jev","answers":{"ok":{"type":"noul","noul":0.9}}},"success":true}`))
	}))
	defer srv.Close()

	c := New(WithBaseURL(srv.URL), WithAPIKey("test"), WithModel("typesafe/jev"),
		WithJevVia(JevViaCloudflare), WithTimeout(5*time.Second))
	out, err := c.Decide(context.Background(), DecideRequest{
		State:     map[string]any{"message": "hi"},
		Questions: map[string]Question{"ok": {Type: "noul", Instructions: "Is this a test?"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Answers["ok"].Noul != 0.9 {
		t.Fatalf("ok = %+v, want the result envelope unwrapped", out.Answers["ok"])
	}
}

func TestJevDefaultsToSystemOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/systemone" {
			http.Error(w, "wrong path "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{},"usage":{}}`))
	}))
	defer srv.Close()

	c := New(WithBaseURL(srv.URL), WithModel("m"), WithTimeout(5*time.Second))
	if c.jevVia != JevViaSystemOne {
		t.Fatalf("jevVia = %q, want systemone by default", c.jevVia)
	}
	if _, err := c.Decide(context.Background(), DecideRequest{}); err != nil {
		t.Fatal(err)
	}
}

func TestJevProtocolInference(t *testing.T) {
	if New(WithBaseURL("https://api.typesafe.ai/v1")).Protocol() != ProtocolJev {
		t.Fatal("typesafe host must infer ProtocolJev")
	}
}

func TestChatRejectsJevProtocol(t *testing.T) {
	c := New(WithBaseURL("https://x.example/v1"), WithProtocol(ProtocolJev), WithModel("m"))
	_, err := c.Chat(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hi"}}})
	if errs.KindOf(err) != errs.KindInvalidArgument {
		t.Fatalf("err = %v, want KindInvalidArgument (Jev serves decisions, not conversations)", err)
	}
}

func TestJevMissingModel(t *testing.T) {
	c := New(WithBaseURL("https://x.example/v1"))
	_, err := c.Decide(context.Background(), DecideRequest{})
	if errs.KindOf(err) != errs.KindInvalidArgument {
		t.Fatalf("err = %v, want KindInvalidArgument", err)
	}
}

func TestJevHTTPErrorMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"slow down"}`, http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := New(WithBaseURL(srv.URL), WithModel("m"), WithTimeout(5*time.Second))
	_, err := c.Decide(context.Background(), DecideRequest{})
	if errs.KindOf(err) != errs.KindRateLimited {
		t.Fatalf("err = %v, want KindRateLimited", err)
	}
}
