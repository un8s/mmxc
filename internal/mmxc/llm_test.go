package mmxc

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBuildLLMRequestShape(t *testing.T) {
	t.Run("text prompt carries a bare string content", func(t *testing.T) {
		req := buildLLMRequest("m", "hello", "", nil, llmOptions{})
		if got, ok := req.Messages[0].Content.(string); !ok || got != "hello" {
			t.Fatalf("content = %#v, want the prompt string", req.Messages[0].Content)
		}
		if req.ResponseFormat != nil {
			t.Fatal("response_format must be omitted outside JSON mode")
		}
		if req.Temperature != nil {
			t.Fatal("temperature must be omitted when unset")
		}
		if req.ReasoningEffort != "" {
			t.Fatal("reasoning_effort must be omitted when unset")
		}
		if req.Stream {
			t.Fatal("stream must be false")
		}
	})

	t.Run("json mode and an explicit zero temperature are sent", func(t *testing.T) {
		req := buildLLMRequest("m", "hi", "none", nil, llmOptions{Temperature: temperature(0), JSONMode: true})
		if req.ResponseFormat == nil || req.ResponseFormat.Type != "json_object" {
			t.Fatalf("response_format = %#v", req.ResponseFormat)
		}
		if req.Temperature == nil || *req.Temperature != 0 {
			t.Fatalf("temperature = %#v, want an explicit 0", req.Temperature)
		}
		if req.ReasoningEffort != "none" {
			t.Fatalf("reasoning_effort = %q", req.ReasoningEffort)
		}
	})

	t.Run("images become data URI content parts", func(t *testing.T) {
		req := buildLLMRequest("m", "read this", "", []llmImage{{MIME: "image/png", Base64: "QUJD"}}, llmOptions{})
		parts, ok := req.Messages[0].Content.([]llmContentPart)
		if !ok || len(parts) != 2 {
			t.Fatalf("content = %#v, want a text part plus an image part", req.Messages[0].Content)
		}
		if parts[0].Type != "text" || parts[0].Text != "read this" {
			t.Fatalf("text part = %#v", parts[0])
		}
		if parts[1].Type != "image_url" || parts[1].ImageURL.URL != "data:image/png;base64,QUJD" {
			t.Fatalf("image part = %#v", parts[1])
		}
	})
}

// llmTestServer returns a client pointed at a stub endpoint, plus a pointer to
// the last request body it received.
func llmTestServer(t *testing.T, apiKey string, handler func(w http.ResponseWriter, body []byte)) (*llmClient, *http.Request, *[]byte) {
	t.Helper()
	var lastReq http.Request
	var lastBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		lastReq = *r
		lastBody = body
		w.Header().Set("Content-Type", "application/json")
		handler(w, body)
	}))
	t.Cleanup(srv.Close)
	cfg := &config{LLMBaseURL: srv.URL + "/v1", LLMAPIKey: apiKey, LLMTimeout: 5 * time.Second}
	return newLLMClient(cfg), &lastReq, &lastBody
}

func TestLLMGenerate(t *testing.T) {
	t.Run("returns the message content and sends the key", func(t *testing.T) {
		client, req, body := llmTestServer(t, "sk-test", func(w http.ResponseWriter, _ []byte) {
			_, _ = io.WriteString(w, `{"choices":[{"finish_reason":"stop","message":{"content":"{\"ok\":true}"}}],"usage":{"prompt_tokens":12}}`)
		})
		got, err := client.generate("m", "hi", nil, llmOptions{JSONMode: true})
		if err != nil {
			t.Fatal(err)
		}
		if got != `{"ok":true}` {
			t.Fatalf("content = %q", got)
		}
		if auth := req.Header.Get("Authorization"); auth != "Bearer sk-test" {
			t.Fatalf("Authorization = %q", auth)
		}
		var sent map[string]any
		if err := json.Unmarshal(*body, &sent); err != nil {
			t.Fatal(err)
		}
		if sent["model"] != "m" {
			t.Fatalf("model = %v", sent["model"])
		}
	})

	t.Run("no key means no Authorization header", func(t *testing.T) {
		client, req, _ := llmTestServer(t, "", func(w http.ResponseWriter, _ []byte) {
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
		})
		if _, err := client.generate("m", "hi", nil, llmOptions{}); err != nil {
			t.Fatal(err)
		}
		if auth := req.Header.Get("Authorization"); auth != "" {
			t.Fatalf("Authorization = %q, want none", auth)
		}
	})

	t.Run("reasoning without content is a diagnosable error", func(t *testing.T) {
		client, _, _ := llmTestServer(t, "", func(w http.ResponseWriter, _ []byte) {
			_, _ = io.WriteString(w, `{"choices":[{"finish_reason":"length","message":{"content":"","reasoning":"thinking..."}}]}`)
		})
		_, err := client.generate("m", "hi", nil, llmOptions{})
		if err == nil || !strings.Contains(err.Error(), "only reasoning") {
			t.Fatalf("err = %v, want a reasoning-only error", err)
		}
	})

	t.Run("a silently truncated prompt is reported", func(t *testing.T) {
		client, _, _ := llmTestServer(t, "", func(w http.ResponseWriter, _ []byte) {
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"[]"}}],"usage":{"prompt_tokens":4096}}`)
		})
		_, err := client.generate("m", strings.Repeat("x", 200000), nil, llmOptions{})
		if err == nil || !strings.Contains(err.Error(), "OLLAMA_CONTEXT_LENGTH") {
			t.Fatalf("err = %v, want a truncation error naming the server setting", err)
		}
	})

	t.Run("an error payload surfaces the message", func(t *testing.T) {
		client, _, _ := llmTestServer(t, "", func(w http.ResponseWriter, _ []byte) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"model not found"}}`)
		})
		_, err := client.generate("m", "hi", nil, llmOptions{})
		if err == nil || !strings.Contains(err.Error(), "model not found") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestLLMListModelsScopesTheAPIKey(t *testing.T) {
	client, req, _ := llmTestServer(t, "sk-test", func(w http.ResponseWriter, _ []byte) {
		_, _ = io.WriteString(w, `{"data":[{"id":"b"},{"id":"a"}]}`)
	})

	models, err := client.listModels(client.cfg.LLMBaseURL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(models, ",") != "a,b" {
		t.Fatalf("models = %v, want them sorted", models)
	}
	if auth := req.Header.Get("Authorization"); auth != "Bearer sk-test" {
		t.Fatalf("configured endpoint should get the key, got %q", auth)
	}

	// Settings lets a caller supply the URL, so the key must not follow it to
	// some other host.
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("key leaked to an unconfigured endpoint: %q", auth)
		}
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer other.Close()
	if _, err := client.listModels(other.URL+"/v1", time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestLLMBaseURLAppendsV1(t *testing.T) {
	cases := map[string]string{
		"http://localhost:11434":       "http://localhost:11434/v1",
		"http://localhost:11434/":      "http://localhost:11434/v1",
		"http://localhost:11434/v1":    "http://localhost:11434/v1",
		"http://localhost:11434/v1/":   "http://localhost:11434/v1",
		"https://openrouter.ai/api/v1": "https://openrouter.ai/api/v1",
		"":                             "",
	}
	for in, want := range cases {
		if got := llmBaseURL(in); got != want {
			t.Errorf("llmBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAIRuntimeStatusDistinguishesUnreachableFromUnlistable(t *testing.T) {
	newStatus := func(t *testing.T, baseURL, model string) aiStatus {
		t.Helper()
		cfg := config{LLMBaseURL: baseURL, LLMModel: model, LLMTimeout: 2 * time.Second}
		a := &app{config: cfg}
		a.llm = newLLMClient(&a.config)
		return a.aiRuntimeStatus(true)
	}

	t.Run("model is served", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"data":[{"id":"m"}]}`)
		}))
		defer srv.Close()
		st := newStatus(t, srv.URL+"/v1", "m")
		if !st.Available || !st.ModelAvailable {
			t.Fatalf("status = %+v, want available", st)
		}
	})

	t.Run("endpoint serves no model list but still works", func(t *testing.T) {
		// OpenRouter-style endpoints may gate or omit /v1/models; refusing to
		// run Ask on that would be wrong, since generate may still succeed.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()
		st := newStatus(t, srv.URL+"/v1", "m")
		if !st.Available || !st.AssistantAvailable {
			t.Fatalf("status = %+v, want available-but-unverified", st)
		}
		if !strings.Contains(st.Detail, "could not list models") {
			t.Fatalf("detail = %q", st.Detail)
		}
	})

	t.Run("endpoint is listed but the model is not", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"data":[{"id":"other"}]}`)
		}))
		defer srv.Close()
		st := newStatus(t, srv.URL+"/v1", "m")
		if st.Available {
			t.Fatalf("status = %+v, want unavailable", st)
		}
	})

	t.Run("unreachable endpoint is unavailable", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL
		srv.Close() // nothing is listening now
		st := newStatus(t, url+"/v1", "m")
		if st.Available {
			t.Fatalf("status = %+v, want unavailable", st)
		}
	})

	t.Run("a malformed endpoint is unavailable", func(t *testing.T) {
		st := newStatus(t, "not-a-url", "m")
		if st.Available {
			t.Fatalf("status = %+v, want unavailable", st)
		}
	})
}
