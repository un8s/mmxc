package mmxc

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	aiStatusCacheTTL = 30 * time.Second
	aiStatusTimeout  = 800 * time.Millisecond
)

type aiStatus struct {
	Configured           bool
	Available            bool
	AssistantAvailable   bool
	ModelAvailable       bool
	TranslationAvailable bool
	Status               string
	Detail               string
	CheckedAt            time.Time
	ConfigKey            string
}

func (a *app) aiRuntimeStatus(refresh bool) aiStatus {
	cfg := a.config
	configured := strings.TrimSpace(cfg.LLMBaseURL) != "" && strings.TrimSpace(cfg.LLMModel) != ""
	key := strings.Join([]string{
		strings.TrimSpace(cfg.LLMBaseURL),
		strings.TrimSpace(cfg.LLMModel),
		strings.TrimSpace(cfg.TranslationModel),
	}, "\x00")
	if !configured {
		return aiStatus{
			Configured: false,
			Available:  false,
			Status:     "AI not configured",
			Detail:     "Set an LLM endpoint and model to enable AI-assisted features.",
			ConfigKey:  key,
		}
	}

	a.mu.Lock()
	cached := a.ai
	if !refresh && cached.ConfigKey == key && !cached.CheckedAt.IsZero() && time.Since(cached.CheckedAt) < aiStatusCacheTTL {
		a.mu.Unlock()
		return cached
	}
	a.mu.Unlock()

	models, err := a.llm.listModels(cfg.LLMBaseURL, aiStatusTimeout)
	status := aiStatus{
		Configured: true,
		CheckedAt:  time.Now(),
		ConfigKey:  key,
	}
	model := strings.TrimSpace(cfg.LLMModel)
	translationModel := strings.TrimSpace(cfg.TranslationModel)

	// Not every OpenAI-compatible endpoint serves /v1/models, and some gate it
	// behind a key we may not have. A listing failure therefore only tells us
	// the endpoint is unreachable when the transport itself failed; otherwise
	// the models are simply unverified, and blocking Ask on that would be wrong.
	switch {
	case errors.As(err, new(*url.Error)), errors.Is(err, errLLMEndpointInvalid):
		status.Status = "AI unavailable"
		status.Detail = err.Error()
	case err != nil:
		status.Available = true
		status.AssistantAvailable = true
		status.ModelAvailable = true
		status.TranslationAvailable = translationModel != ""
		status.Status = "AI available"
		status.Detail = fmt.Sprintf("Endpoint is reachable; could not list models to confirm %q is available.", model)
	default:
		available := make(map[string]bool, len(models))
		for _, name := range models {
			available[name] = true
		}
		status.ModelAvailable = available[model]
		status.TranslationAvailable = translationModel != "" && available[translationModel]
		status.AssistantAvailable = status.ModelAvailable
		status.Available = status.ModelAvailable
		if status.ModelAvailable {
			status.Status = "AI available"
			status.Detail = fmt.Sprintf("LLM endpoint is reachable; model %q is available.", model)
		} else {
			status.Status = "AI unavailable"
			status.Detail = fmt.Sprintf("LLM endpoint is reachable, but model %q is not served.", model)
		}
	}
	a.mu.Lock()
	a.ai = status
	a.mu.Unlock()
	return status
}

func (a *app) assistantUnavailableMessage() string {
	st := a.aiRuntimeStatus(false)
	if !st.Configured {
		return "Ask is disabled because AI is not configured. Set an LLM endpoint and model in Settings."
	}
	if st.Detail != "" {
		return "Ask is disabled because AI is not available: " + st.Detail
	}
	return "Ask is disabled because AI is not available."
}

func (a *app) assistantReady() bool {
	return a.aiRuntimeStatus(false).AssistantAvailable
}
