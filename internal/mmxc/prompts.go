package mmxc

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The Ask and statement-extraction prompts are split in two. The contract —
// the output schema, tool list, allowed values and the injected data — stays in
// Go next to the parser that depends on it (BuildAssistantToolPrompt,
// statementJSONShape, statementFieldRules). The guidance — how to choose tools, read dates,
// or tell statement rows apart — lives in text files that a user can override
// per model or per bank without rebuilding: assets/prompts/<name>.txt is the
// built-in default, and <prompt_dir>/<name>.txt replaces it when present.
//
// An override is read on every use, so an edit takes effect on the next LLM
// call. Leading lines starting with "#" are comments and are not sent to the
// model; one of them carries the version header ("# mmxc-prompt-version: N").
// The built-in version is bumped whenever the guidance changes in a way an
// override should follow (a new tool, a new field), and Settings warns when an
// override was written for another version.

const (
	promptAssistant = "assistant"
	promptStatement = "statement"
)

var promptNames = []string{promptAssistant, promptStatement}

const promptVersionKey = "mmxc-prompt-version:"

// Prompts locates the guidance text of the LLM prompts. Dir holds the optional
// override files; an empty Dir means the built-in text only.
type Prompts struct {
	Dir string
}

// PromptStatus reports which guidance a prompt uses, for Settings and the evals.
type PromptStatus struct {
	Name string
	// Path is the override file, set whenever one exists (even when it could
	// not be used, so the warning can name it).
	Path string
	// Custom is true when the override replaces the built-in guidance.
	Custom  bool
	Warning string
}

// Source names where the guidance comes from, for the eval reports.
func (s PromptStatus) Source() string {
	if s.Custom {
		return "custom (" + s.Path + ")"
	}
	return "built-in"
}

type promptFile struct {
	text    string
	version int // 0 when the file has no version header
}

// builtinPrompts holds the parsed embedded guidance, keyed by prompt name.
var builtinPrompts = func() map[string]promptFile {
	out := make(map[string]promptFile, len(promptNames))
	for _, name := range promptNames {
		data, err := promptAssets.ReadFile("assets/prompts/" + name + ".txt")
		if err != nil {
			panic(fmt.Sprintf("built-in prompt %s: %v", name, err))
		}
		p := parsePromptFile(string(data))
		if p.version == 0 || strings.TrimSpace(p.text) == "" {
			panic(fmt.Sprintf("built-in prompt %s needs a %s header and guidance text", name, promptVersionKey))
		}
		out[name] = p
	}
	return out
}()

// builtinPromptFile returns the embedded file for name exactly as shipped,
// header included, so it can be downloaded as the starting point of an override.
func builtinPromptFile(name string) ([]byte, bool) {
	if _, ok := builtinPrompts[name]; !ok {
		return nil, false
	}
	data, err := promptAssets.ReadFile("assets/prompts/" + name + ".txt")
	return data, err == nil
}

// parsePromptFile strips the leading comment lines and reads the version header
// from them.
func parsePromptFile(raw string) promptFile {
	var p promptFile
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	i := 0
	for ; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "#") {
			break
		}
		comment := strings.TrimSpace(strings.TrimPrefix(line, "#"))
		if value, ok := strings.CutPrefix(comment, promptVersionKey); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && n > 0 {
				p.version = n
			}
		}
	}
	p.text = strings.TrimSpace(strings.Join(lines[i:], "\n"))
	return p
}

// guidance returns the guidance text for name: the override in Dir when it
// exists and is usable, the built-in text otherwise.
func (p Prompts) guidance(name string) string {
	text, _ := p.load(name)
	return text
}

// Status reports, for every overridable prompt, whether an override is in use
// and anything wrong with it.
func (p Prompts) Status() []PromptStatus {
	out := make([]PromptStatus, 0, len(promptNames))
	for _, name := range promptNames {
		_, status := p.load(name)
		out = append(out, status)
	}
	return out
}

// AssistantStatus and StatementStatus report on one prompt each, for the evals.
func (p Prompts) AssistantStatus() PromptStatus {
	_, status := p.load(promptAssistant)
	return status
}

func (p Prompts) StatementStatus() PromptStatus {
	_, status := p.load(promptStatement)
	return status
}

func (p Prompts) load(name string) (string, PromptStatus) {
	builtin := builtinPrompts[name]
	status := PromptStatus{Name: name}
	if strings.TrimSpace(p.Dir) == "" {
		return builtin.text, status
	}
	path := filepath.Join(p.Dir, name+".txt")
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return builtin.text, status
	}
	status.Path = path
	if err != nil {
		status.Warning = fmt.Sprintf("%s could not be read, so the built-in prompt is used: %v", path, err)
		return builtin.text, status
	}
	custom := parsePromptFile(string(data))
	if custom.text == "" {
		status.Warning = fmt.Sprintf("%s is empty, so the built-in prompt is used", path)
		return builtin.text, status
	}
	status.Custom = true
	switch {
	case custom.version == 0:
		status.Warning = fmt.Sprintf("%s has no \"# %s\" header; the built-in prompt is version %d", path, promptVersionKey, builtin.version)
	case custom.version != builtin.version:
		status.Warning = fmt.Sprintf("%s was written for prompt version %d, but the built-in prompt is now version %d; compare it with the built-in and update the header", path, custom.version, builtin.version)
	}
	return custom.text, status
}

func (a *app) prompts() Prompts {
	return Prompts{Dir: a.config.PromptDir}
}

// downloadBuiltinPrompt serves a built-in prompt file as shipped, the starting
// point for an override in prompt_dir.
func (a *app) downloadBuiltinPrompt(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	data, ok := builtinPromptFile(name)
	if !ok {
		a.httpError(w, "unknown prompt", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.txt"`)
	_, _ = w.Write(data)
}
