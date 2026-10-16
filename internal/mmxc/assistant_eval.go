package mmxc

import "encoding/json"

// This file exposes the assistant decision parser to the assistant eval
// (cmd/assistant-eval) so the eval reuses the app's exact prompt
// (BuildAssistantToolPrompt) and parser instead of hand-synced copies that could
// drift. See extraction_eval.go for the same single-sourcing pattern applied to
// statement extraction.

// AssistantDecision is the normalized assistant tool-selection decision exposed
// for the eval. Arguments is the decoded tool-argument object (or nil when the
// model returned none).
type AssistantDecision struct {
	Action    string
	Tool      string
	Arguments map[string]any
	Answer    string
}

// ParseAssistantToolDecision parses a raw model response into the normalized
// decision exactly as the app does (parseAssistantToolDecision +
// normalizeAssistantToolDecision), including the legacy ask_clarification
// fallback and the tool-name-as-top-level-action normalization.
func ParseAssistantToolDecision(raw string) (AssistantDecision, error) {
	decision, err := parseAssistantToolDecision(raw)
	if err != nil {
		return AssistantDecision{}, err
	}
	out := AssistantDecision{Action: decision.Action, Tool: decision.Tool, Answer: decision.Answer}
	if len(decision.Arguments) > 0 {
		// arguments is normally a JSON object; leave it nil if it is not.
		_ = json.Unmarshal(decision.Arguments, &out.Arguments)
	}
	return out, nil
}
