package mmxc

import "time"

// This file exports the app's LLM transport to the eval commands under cmd/, so
// they exercise exactly the request the app sends instead of hand-rolling their
// own HTTP client. It is the transport counterpart of the prompt/parser exports
// in assistant_eval.go and extraction_eval.go: nothing here to keep in sync.

// LLMSettings is the eval-facing subset of the LLM configuration.
type LLMSettings struct {
	BaseURL         string
	APIKey          string
	ReasoningEffort string
	Timeout         time.Duration
	LogPrompts      bool
}

// LLMResult is one completion, including the fields an eval reports on when a
// model fails to answer.
type LLMResult struct {
	Content      string
	Reasoning    string
	FinishReason string
}

// EvalLLMClient is an LLM client built from explicit settings rather than from
// a loaded config file, since the evals take their endpoint from flags.
type EvalLLMClient struct {
	inner *llmClient
}

func NewEvalLLMClient(s LLMSettings) *EvalLLMClient {
	cfg := &config{
		LLMBaseURL:         s.BaseURL,
		LLMAPIKey:          s.APIKey,
		LLMReasoningEffort: s.ReasoningEffort,
		LLMTimeout:         s.Timeout,
		LogLLM:             s.LogPrompts,
	}
	return &EvalLLMClient{inner: newLLMClient(cfg)}
}

// Generate runs one completion. imagesBase64 entries are paired with mimeTypes
// of the same length; pass nil for text-only prompts.
func (c *EvalLLMClient) Generate(model, prompt string, imagesBase64, mimeTypes []string, jsonMode bool, temp float64) (LLMResult, error) {
	images := make([]llmImage, 0, len(imagesBase64))
	for i, data := range imagesBase64 {
		mime := "image/jpeg"
		if i < len(mimeTypes) && mimeTypes[i] != "" {
			mime = mimeTypes[i]
		}
		images = append(images, llmImage{MIME: mime, Base64: data})
	}
	res, err := c.inner.complete(model, prompt, images, llmOptions{
		Temperature: temperature(temp),
		JSONMode:    jsonMode,
	})
	return LLMResult{Content: res.Content, Reasoning: res.Reasoning, FinishReason: res.FinishReason}, err
}

// ListModels reports the model ids the configured endpoint serves.
func (c *EvalLLMClient) ListModels() ([]string, error) {
	return c.inner.listModels(c.inner.cfg.LLMBaseURL, 10*time.Second)
}
