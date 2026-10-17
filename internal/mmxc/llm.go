package mmxc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// This file is the app's only LLM transport. It speaks the OpenAI
// chat-completions API, which Ollama, OpenRouter, OpenAI, vLLM, LM Studio and
// llama.cpp all serve, so there is deliberately no provider abstraction here:
// one client, one request shape, one response shape.
//
// The endpoint is a base URL ending in /v1 (e.g. http://localhost:11434/v1 for
// Ollama). Unlike Ollama's native API there is no per-request context-window
// parameter — the window is a server-side setting (OLLAMA_CONTEXT_LENGTH for
// Ollama) — so oversized prompts are silently truncated by the server rather
// than rejected. detectPromptTruncation below turns that silence into an error.

// llmImage is one inline image sent with a prompt, carried as base64 plus the
// MIME type needed to build the data: URI the API expects.
type llmImage struct {
	MIME   string
	Base64 string
}

// llmOptions are the per-call knobs. Temperature is a pointer so that an
// explicit 0 (used for deterministic repair and translation) is distinguishable
// from "unset".
type llmOptions struct {
	Temperature *float64
	JSONMode    bool
}

func temperature(v float64) *float64 { return &v }

// llmClient reads its settings (endpoint, key, timeout) at call time, so a
// Settings change applies without recreating the client. The config pointer is
// swapped as a whole (setConfig), never written through, because requests use
// the client concurrently.
type llmClient struct {
	cfg atomic.Pointer[config]
}

func newLLMClient(cfg *config) *llmClient {
	c := &llmClient{}
	c.cfg.Store(cfg)
	return c
}

func (c *llmClient) setConfig(cfg *config) { c.cfg.Store(cfg) }

// errLLMEndpointInvalid marks a misconfigured endpoint, as opposed to a
// reachable one that merely declined a request. AI status treats the two
// differently: the first is "unavailable", the second only "unverified".
var errLLMEndpointInvalid = errors.New("LLM endpoint is not usable")

type llmContentPart struct {
	Type     string       `json:"type"`
	Text     string       `json:"text,omitempty"`
	ImageURL *llmImageURL `json:"image_url,omitempty"`
}

type llmImageURL struct {
	URL string `json:"url"`
}

type llmMessage struct {
	Role string `json:"role"`
	// Content is a plain string for text-only prompts and a []llmContentPart
	// when images are attached, matching the OpenAI content-parts shape.
	Content any `json:"content"`
}

type llmResponseFormat struct {
	Type string `json:"type"`
}

type llmRequest struct {
	Model           string             `json:"model"`
	Messages        []llmMessage       `json:"messages"`
	Stream          bool               `json:"stream"`
	Temperature     *float64           `json:"temperature,omitempty"`
	ResponseFormat  *llmResponseFormat `json:"response_format,omitempty"`
	ReasoningEffort string             `json:"reasoning_effort,omitempty"`
}

type llmResponse struct {
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content string `json:"content"`
			// Reasoning is non-standard but widely returned (Ollama,
			// OpenRouter) when a thinking model spends its budget without
			// producing an answer. Seeing it lets us say so precisely.
			Reasoning string `json:"reasoning"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// buildLLMRequest assembles the request body. Split out from generate so the
// wire shape can be asserted in tests without a server.
func buildLLMRequest(model, prompt, reasoningEffort string, images []llmImage, opts llmOptions) llmRequest {
	msg := llmMessage{Role: "user", Content: prompt}
	if len(images) > 0 {
		parts := []llmContentPart{{Type: "text", Text: prompt}}
		for _, img := range images {
			parts = append(parts, llmContentPart{
				Type:     "image_url",
				ImageURL: &llmImageURL{URL: "data:" + img.MIME + ";base64," + img.Base64},
			})
		}
		msg.Content = parts
	}
	req := llmRequest{
		Model:           model,
		Messages:        []llmMessage{msg},
		Stream:          false,
		Temperature:     opts.Temperature,
		ReasoningEffort: strings.TrimSpace(reasoningEffort),
	}
	if opts.JSONMode {
		req.ResponseFormat = &llmResponseFormat{Type: "json_object"}
	}
	return req
}

// llmResult is one completion. Reasoning and FinishReason are surfaced because
// the evals report on them and because an empty Content with a non-empty
// Reasoning is a distinct, diagnosable failure.
type llmResult struct {
	Content      string
	Reasoning    string
	FinishReason string
}

// generate runs one completion and returns just the message content, which is
// all four in-app call sites need.
func (c *llmClient) generate(model, prompt string, images []llmImage, opts llmOptions) (string, error) {
	res, err := c.complete(model, prompt, images, opts)
	return res.Content, err
}

// complete runs one non-streaming chat completion.
func (c *llmClient) complete(model, prompt string, images []llmImage, opts llmOptions) (llmResult, error) {
	cfg := c.cfg.Load()
	base := llmBaseURL(cfg.LLMBaseURL)
	if base == "" {
		return llmResult{}, errors.New("LLM endpoint is not configured")
	}
	if strings.TrimSpace(model) == "" {
		return llmResult{}, errors.New("LLM model is not configured")
	}
	payload, err := json.Marshal(buildLLMRequest(model, prompt, cfg.LLMReasoningEffort, images, opts))
	if err != nil {
		return llmResult{}, err
	}
	req, err := http.NewRequest(http.MethodPost, base+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return llmResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key := strings.TrimSpace(cfg.LLMAPIKey); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	timeout := cfg.LLMTimeout
	if timeout <= 0 {
		timeout = 4 * time.Minute
	}
	log.Printf("llm request: model=%s prompt=%dB images=%d json=%t timeout=%s",
		model, len(prompt), len(images), opts.JSONMode, timeout)
	if cfg.LogLLM {
		log.Printf("llm prompt:\n%s", prompt)
	}

	client := &http.Client{Timeout: timeout}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return llmResult{}, fmt.Errorf("call LLM: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return llmResult{}, err
	}
	elapsed := time.Since(start).Round(time.Millisecond)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("llm response: status=%s elapsed=%s body=%s", resp.Status, elapsed, logExcerpt(string(data), 2000))
		return llmResult{}, fmt.Errorf("LLM returned %s: %s", resp.Status, short(string(data)))
	}
	var out llmResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return llmResult{}, fmt.Errorf("parse LLM response: %w", err)
	}
	if out.Error != nil && out.Error.Message != "" {
		log.Printf("llm response: error=%q elapsed=%s", out.Error.Message, elapsed)
		return llmResult{}, errors.New(out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return llmResult{}, errors.New("LLM returned no choices")
	}
	choice := out.Choices[0]
	content := choice.Message.Content
	log.Printf("llm response: ok elapsed=%s content=%dB reasoning=%dB finish_reason=%s prompt_tokens=%d",
		elapsed, len(content), len(choice.Message.Reasoning), choice.FinishReason, out.Usage.PromptTokens)
	if cfg.LogLLM {
		log.Printf("llm raw response:\n%s", content)
		if strings.TrimSpace(choice.Message.Reasoning) != "" {
			log.Printf("llm raw reasoning:\n%s", choice.Message.Reasoning)
		}
	}
	res := llmResult{Content: content, Reasoning: choice.Message.Reasoning, FinishReason: choice.FinishReason}
	if err := detectPromptTruncation(len(prompt), len(images), out.Usage.PromptTokens); err != nil {
		return res, err
	}
	// An empty answer with only reasoning means the model never finished
	// thinking. Lower llm_reasoning_effort (or set it to "none") or give the
	// server a larger context window.
	if strings.TrimSpace(content) == "" && strings.TrimSpace(choice.Message.Reasoning) != "" {
		return llmResult{}, fmt.Errorf("LLM returned only reasoning and no answer (finish_reason=%s); lower llm_reasoning_effort or raise the server's context window", choice.FinishReason)
	}
	return res, nil
}

// listModels asks an endpoint what it serves. The URL is a parameter rather
// than read from config because Settings queries the endpoint the user is
// currently typing, before it has been saved.
func (c *llmClient) listModels(rawURL string, timeout time.Duration) ([]string, error) {
	cfg := c.cfg.Load()
	base := llmBaseURL(rawURL)
	if base == "" {
		return nil, fmt.Errorf("%w: no base URL is configured", errLLMEndpointInvalid)
	}
	if err := validateHTTPURL(base, "LLM base URL"); err != nil {
		return nil, fmt.Errorf("%w: %s", errLLMEndpointInvalid, err)
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	req, err := http.NewRequest(http.MethodGet, base+"/models", nil)
	if err != nil {
		return nil, err
	}
	// Only ever attach the key to the endpoint it belongs to: Settings passes a
	// caller-supplied URL here, and sending the key to an arbitrary host would
	// leak it.
	if key := strings.TrimSpace(cfg.LLMAPIKey); key != "" && base == llmBaseURL(cfg.LLMBaseURL) {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query LLM endpoint: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("LLM endpoint returned %s: %s", resp.Status, short(string(data)))
	}
	return parseLLMModels(data)
}

func parseLLMModels(data []byte) ([]string, error) {
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse LLM model list: %w", err)
	}
	seen := make(map[string]bool, len(out.Data))
	var models []string
	for _, model := range out.Data {
		id := strings.TrimSpace(model.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		models = append(models, id)
	}
	sort.Strings(models)
	return models, nil
}

// A prompt that overflows the server's context window is silently truncated
// rather than rejected, which would quietly corrupt a statement extraction. We
// cannot set the window over this API, but the response reports how many prompt
// tokens the server actually processed, so a count far below what we sent is
// proof of truncation.
const (
	// Rough bytes per token for statement prose plus JSON punctuation. Slightly
	// pessimistic (English averages nearer 4), which keeps the check quiet.
	llmBytesPerToken = 3.5
	// A base64 image costs far more than its byte count suggests, so budget a
	// flat slice per attached image instead of measuring it.
	llmTokensPerImage = 1536
	// Below this the window is never the problem, and the ratio gets noisy.
	llmTruncationFloor = 2000
)

// estimatePromptTokens approximates the token cost of a prompt plus its images.
func estimatePromptTokens(promptBytes, images int) int {
	return int(float64(promptBytes)/llmBytesPerToken) + images*llmTokensPerImage
}

// detectPromptTruncation reports an actionable error when the server processed
// far fewer prompt tokens than we sent it.
func detectPromptTruncation(promptBytes, images, reportedTokens int) error {
	estimated := estimatePromptTokens(promptBytes, images)
	if reportedTokens <= 0 || estimated < llmTruncationFloor {
		return nil
	}
	if reportedTokens >= estimated/2 {
		return nil
	}
	return fmt.Errorf("the LLM server truncated the prompt: sent about %d tokens but only %d were processed; raise the server's context window (for Ollama, OLLAMA_CONTEXT_LENGTH)", estimated, reportedTokens)
}

// llmBaseURL normalizes a configured endpoint: trailing slashes removed, and
// the OpenAI-conventional /v1 appended when the user pasted a bare host (a
// common paste for Ollama, whose native URL carries no path).
func llmBaseURL(raw string) string {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if u.Path == "" {
		return raw + "/v1"
	}
	return raw
}
