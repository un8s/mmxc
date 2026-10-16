package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/un8s/mmxc/internal/mmxc"
)

type evalCase struct {
	Name            string             `json:"name"`
	Question        string             `json:"question"`
	Transcript      []map[string]any   `json:"transcript"`
	Expect          expectedDecision   `json:"expect"`
	AnyOf           []expectedDecision `json:"any_of"`
	ExpectedFailure string             `json:"expected_failure"`
}

type expectedDecision struct {
	Action         string         `json:"action"`
	Tool           string         `json:"tool"`
	Arguments      map[string]any `json:"arguments"`
	Absent         []string       `json:"absent"`
	AnswerContains []string       `json:"answer_contains"`
}

type modelDecision struct {
	Action    string         `json:"action"`
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
	Answer    string         `json:"answer"`
}

type evalResult struct {
	Model            string
	Case             string
	Passed           bool
	Latency          time.Duration
	DoneReason       string
	Decision         modelDecision
	Errors           []string
	Prompt           string
	Raw              string
	Thinking         string
	ExpectedFailure  string
	UnexpectedlyPass bool
}

type countFlag int

func (c *countFlag) String() string {
	return strconv.Itoa(int(*c))
}

func (c *countFlag) Set(value string) error {
	*c = *c + 1
	return nil
}

func (c *countFlag) IsBoolFlag() bool {
	return true
}

func main() {
	var modelsText string
	var baseURL string
	var apiKey string
	var reasoningEffort string
	var casesPath string
	var accounts string
	var currentDate string
	var timeoutText string
	var temperature float64
	var promptDir string
	var verbose countFlag
	flag.StringVar(&modelsText, "models", "qwen3:30b", "comma-separated model names")
	flag.StringVar(&baseURL, "url", "http://localhost:11434/v1", "OpenAI-compatible base URL")
	flag.StringVar(&apiKey, "api-key", os.Getenv("MMXC_LLM_API_KEY"), "bearer token for the endpoint (default $MMXC_LLM_API_KEY)")
	flag.StringVar(&reasoningEffort, "reasoning-effort", "none", "reasoning_effort: none, low, medium, high, or max")
	flag.StringVar(&casesPath, "cases", "docs/assistant_eval_cases.json", "assistant eval cases JSON")
	flag.StringVar(&accounts, "accounts", "Primary Checking, Rewards Card, Shared Savings", "account names injected into the prompt")
	flag.StringVar(&currentDate, "current-date", time.Now().Format("2006-01-02"), "current date injected into the prompt")
	flag.StringVar(&timeoutText, "timeout", "120s", "per-call timeout")
	flag.Float64Var(&temperature, "temperature", 0, "sampling temperature")
	flag.StringVar(&promptDir, "prompt-dir", "data/prompts", "directory with an assistant.txt override of the built-in prompt guidance, as the app's prompt_dir; empty grades the built-in")
	flag.Var(&verbose, "verbose", "print per-case details; repeat for rendered prompt and raw model response")
	flag.Parse()

	timeout, err := time.ParseDuration(timeoutText)
	if err != nil {
		fatalf("parse timeout: %v", err)
	}
	models := splitCSV(modelsText)
	if len(models) == 0 {
		fatalf("at least one model is required")
	}
	cases, err := loadCases(casesPath)
	if err != nil {
		fatalf("load cases: %v", err)
	}
	prompts := mmxc.Prompts{Dir: promptDir}
	// Say which guidance is graded, so a score from an override is never
	// mistaken for the built-in prompt's.
	status := prompts.AssistantStatus()
	fmt.Printf("assistant prompt: %s\n", status.Source())
	if status.Warning != "" {
		fmt.Printf("warning: %s\n", status.Warning)
	}

	// Reuse the app's own transport so the eval sends exactly the request the
	// app sends, with no second HTTP client to keep in sync.
	client := mmxc.NewEvalLLMClient(mmxc.LLMSettings{
		BaseURL:         baseURL,
		APIKey:          apiKey,
		ReasoningEffort: reasoningEffort,
		Timeout:         timeout,
	})
	var all []evalResult
	for _, model := range models {
		for _, tc := range cases {
			result := runCase(client, prompts, model, accounts, currentDate, temperature, tc)
			all = append(all, result)
			if verbose > 0 || !result.Passed {
				printCase(result, int(verbose))
			}
		}
	}
	printSummary(all)
	if failedCount(all) > 0 {
		os.Exit(1)
	}
}

func loadCases(path string) ([]evalCase, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cases []evalCase
	if err := json.Unmarshal(data, &cases); err != nil {
		return nil, err
	}
	if len(cases) == 0 {
		return nil, errors.New("no eval cases found")
	}
	for i := range cases {
		if strings.TrimSpace(cases[i].Name) == "" {
			return nil, fmt.Errorf("case %d is missing name", i+1)
		}
		if strings.TrimSpace(cases[i].Question) == "" {
			return nil, fmt.Errorf("case %s is missing question", cases[i].Name)
		}
	}
	return cases, nil
}

func runCase(client *mmxc.EvalLLMClient, prompts mmxc.Prompts, model, accounts, currentDate string, temperature float64, tc evalCase) evalResult {
	start := time.Now()
	// Reuse the app's exact prompt builder so the eval can never drift from the
	// production prompt (no copied template). The accounts CSV and current date
	// stand in for the DB/clock the app uses.
	prompt := mmxc.BuildAssistantToolPrompt(prompts, currentDate, splitCSV(accounts), tc.Question, tc.Transcript)
	res, err := client.Generate(model, prompt, nil, nil, true, temperature)
	raw := res.Content
	result := evalResult{Model: model, Case: tc.Name, Latency: time.Since(start).Round(time.Millisecond), DoneReason: res.FinishReason, Prompt: prompt, Raw: raw, Thinking: res.Reasoning, ExpectedFailure: strings.TrimSpace(tc.ExpectedFailure)}
	if err != nil {
		result.Errors = []string{err.Error()}
		return result
	}
	decision, err := parseDecision(raw)
	result.Decision = decision
	if err != nil {
		result.Errors = []string{err.Error()}
		return result
	}
	result.Errors = scoreCase(tc, decision, currentDate)
	if result.ExpectedFailure != "" {
		result.UnexpectedlyPass = len(result.Errors) == 0
		result.Passed = true
	} else {
		result.Passed = len(result.Errors) == 0
	}
	return result
}

func parseDecision(raw string) (modelDecision, error) {
	// Reuse the app's parser + normalizer so the eval grades exactly what the app
	// would parse — including the legacy ask_clarification shape and the
	// tool-name-as-top-level-action normalization — with no duplicated logic to
	// keep in sync.
	dec, err := mmxc.ParseAssistantToolDecision(raw)
	if err != nil {
		return modelDecision{}, err
	}
	return modelDecision{Action: dec.Action, Tool: dec.Tool, Arguments: dec.Arguments, Answer: dec.Answer}, nil
}

func scoreDecision(expect expectedDecision, got modelDecision, dates map[string]string) []string {
	expect = resolveExpectedDecision(expect, dates)
	got.Arguments = normalizeArgumentAliases(got.Arguments)
	var errs []string
	if expect.Action != "" && got.Action != expect.Action {
		errs = append(errs, fmt.Sprintf("action got %q want %q", got.Action, expect.Action))
	}
	if expect.Tool != "" && got.Tool != expect.Tool {
		errs = append(errs, fmt.Sprintf("tool got %q want %q", got.Tool, expect.Tool))
	}
	for key, want := range expect.Arguments {
		gotValue, ok := got.Arguments[key]
		if !ok {
			errs = append(errs, fmt.Sprintf("argument %s missing, want %s", key, valueString(want)))
			continue
		}
		if !valuesEqual(gotValue, want) {
			errs = append(errs, fmt.Sprintf("argument %s got %s want %s", key, valueString(gotValue), valueString(want)))
		}
	}
	for _, key := range expect.Absent {
		if value, ok := got.Arguments[key]; ok && !isEmptyArgument(value) {
			errs = append(errs, fmt.Sprintf("argument %s got %s, want absent/empty", key, valueString(value)))
		}
	}
	answerFolded := strings.ToLower(got.Answer)
	for _, want := range expect.AnswerContains {
		if !strings.Contains(answerFolded, strings.ToLower(want)) {
			errs = append(errs, fmt.Sprintf("answer got %q, want to contain %q", got.Answer, want))
		}
	}
	if got.Action == "tool" && strings.TrimSpace(got.Answer) != "" {
		errs = append(errs, fmt.Sprintf("tool answer got %q, want empty", got.Answer))
	}
	return errs
}

func normalizeArgumentAliases(args map[string]any) map[string]any {
	if len(args) == 0 {
		return args
	}
	normalized := make(map[string]any, len(args)+1)
	for key, value := range args {
		normalized[key] = value
	}
	if isEmptyArgument(normalized["description_contains"]) && !isEmptyArgument(normalized["search"]) {
		normalized["description_contains"] = normalized["search"]
	}
	if isEmptyArgument(normalized["search"]) && !isEmptyArgument(normalized["description_contains"]) {
		normalized["search"] = normalized["description_contains"]
	}
	return normalized
}

func scoreCase(tc evalCase, got modelDecision, currentDate string) []string {
	dates, err := evalDatePlaceholders(currentDate)
	if err != nil {
		return []string{err.Error()}
	}
	if len(tc.AnyOf) == 0 {
		return scoreDecision(tc.Expect, got, dates)
	}
	var best []string
	for _, expect := range tc.AnyOf {
		errs := scoreDecision(expect, got, dates)
		if len(errs) == 0 {
			return nil
		}
		if best == nil || len(errs) < len(best) {
			best = errs
		}
	}
	if len(best) == 0 {
		return []string{"no any_of expectation matched"}
	}
	return append([]string{"no any_of expectation matched; closest mismatch:"}, best...)
}

func resolveExpectedDecision(expect expectedDecision, dates map[string]string) expectedDecision {
	if len(expect.Arguments) > 0 {
		args := make(map[string]any, len(expect.Arguments))
		for key, value := range expect.Arguments {
			args[key] = resolveExpectedValue(value, dates)
		}
		expect.Arguments = args
	}
	if len(expect.AnswerContains) > 0 {
		values := make([]string, len(expect.AnswerContains))
		for i, value := range expect.AnswerContains {
			values[i] = replaceDatePlaceholders(value, dates)
		}
		expect.AnswerContains = values
	}
	return expect
}

func resolveExpectedValue(value any, dates map[string]string) any {
	switch v := value.(type) {
	case string:
		return replaceDatePlaceholders(v, dates)
	case []any:
		values := make([]any, len(v))
		for i := range v {
			values[i] = resolveExpectedValue(v[i], dates)
		}
		return values
	case map[string]any:
		values := make(map[string]any, len(v))
		for key, item := range v {
			values[key] = resolveExpectedValue(item, dates)
		}
		return values
	default:
		return value
	}
}

func replaceDatePlaceholders(value string, dates map[string]string) string {
	for key, replacement := range dates {
		value = strings.ReplaceAll(value, "{{"+key+"}}", replacement)
	}
	return value
}

func evalDatePlaceholders(currentDate string) (map[string]string, error) {
	current, err := time.Parse("2006-01-02", currentDate)
	if err != nil {
		return nil, fmt.Errorf("parse current date %q: %w", currentDate, err)
	}
	currentYear, currentMonth, _ := current.Date()
	currentMonthStart := time.Date(currentYear, currentMonth, 1, 0, 0, 0, 0, current.Location())
	currentMonthEnd := currentMonthStart.AddDate(0, 1, -1)
	lastMonthStart := currentMonthStart.AddDate(0, -1, 0)
	lastMonthEnd := currentMonthStart.AddDate(0, 0, -1)
	currentYearStart := time.Date(currentYear, 1, 1, 0, 0, 0, 0, current.Location())
	currentYearEnd := time.Date(currentYear, 12, 31, 0, 0, 0, 0, current.Location())
	lastYearStart := currentYearStart.AddDate(-1, 0, 0)
	lastYearEnd := currentYearStart.AddDate(0, 0, -1)
	return map[string]string{
		"current_date":        current.Format("2006-01-02"),
		"current_year":        current.Format("2006"),
		"last_year":           current.AddDate(-1, 0, 0).Format("2006"),
		"current_month_start": currentMonthStart.Format("2006-01-02"),
		"current_month_end":   currentMonthEnd.Format("2006-01-02"),
		"last_month_start":    lastMonthStart.Format("2006-01-02"),
		"last_month_end":      lastMonthEnd.Format("2006-01-02"),
		"current_year_start":  currentYearStart.Format("2006-01-02"),
		"current_year_end":    currentYearEnd.Format("2006-01-02"),
		"last_year_start":     lastYearStart.Format("2006-01-02"),
		"last_year_end":       lastYearEnd.Format("2006-01-02"),
	}, nil
}

func valuesEqual(got, want any) bool {
	if alternatives, ok := anyOfValues(want); ok {
		for _, alternative := range alternatives {
			if valuesEqual(got, alternative) {
				return true
			}
		}
		return false
	}
	gotNumber, gotOK := numberValue(got)
	wantNumber, wantOK := numberValue(want)
	if gotOK && wantOK {
		return math.Abs(gotNumber-wantNumber) < 0.000001
	}
	return reflect.DeepEqual(got, want)
}

func anyOfValues(value any) ([]any, bool) {
	fields, ok := value.(map[string]any)
	if !ok || len(fields) != 1 {
		return nil, false
	}
	raw, ok := fields["any_of"]
	if !ok {
		return nil, false
	}
	values, ok := raw.([]any)
	return values, ok
}

func numberValue(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case json.Number:
		n, err := strconv.ParseFloat(string(v), 64)
		return n, err == nil
	default:
		return 0, false
	}
}

func isEmptyArgument(value any) bool {
	switch v := value.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(v) == ""
	case float64:
		return v == 0
	case bool:
		return !v
	default:
		return false
	}
}

func printCase(result evalResult, verbose int) {
	status := "PASS"
	switch {
	case !result.Passed:
		status = "FAIL"
	case result.ExpectedFailure != "" && result.UnexpectedlyPass:
		status = "XPASS"
	case result.ExpectedFailure != "":
		status = "XFAIL"
	}
	fmt.Printf("%s %-24s %-36s %s done=%s\n", status, result.Model, result.Case, result.Latency, result.DoneReason)
	if result.ExpectedFailure != "" {
		fmt.Printf("  expected failure: %s\n", result.ExpectedFailure)
	}
	if len(result.Errors) > 0 {
		for _, errText := range result.Errors {
			fmt.Printf("  - %s\n", errText)
		}
	}
	data, _ := json.Marshal(result.Decision)
	if len(data) > 0 && string(data) != "{}" {
		fmt.Printf("  decision: %s\n", data)
	}
	if verbose >= 2 {
		fmt.Println("  prompt:")
		printIndented(result.Prompt, "    ")
		if strings.TrimSpace(result.Thinking) != "" {
			fmt.Println("  thinking:")
			printIndented(result.Thinking, "    ")
		}
		fmt.Println("  raw response:")
		printIndented(result.Raw, "    ")
	}
}

func printIndented(text, prefix string) {
	if text == "" {
		fmt.Println(prefix)
		return
	}
	for _, line := range strings.Split(text, "\n") {
		fmt.Println(prefix + line)
	}
}

func printSummary(results []evalResult) {
	byModel := make(map[string][]evalResult)
	for _, result := range results {
		byModel[result.Model] = append(byModel[result.Model], result)
	}
	var models []string
	for model := range byModel {
		models = append(models, model)
	}
	sort.Strings(models)
	fmt.Println()
	fmt.Println("Model\tPassed\tFailed\tXFail\tXPass\tMean latency")
	for _, model := range models {
		results := byModel[model]
		failed := 0
		xfail := 0
		xpass := 0
		passed := 0
		var total time.Duration
		for _, result := range results {
			switch {
			case !result.Passed:
				failed++
			case result.ExpectedFailure != "" && result.UnexpectedlyPass:
				xpass++
			case result.ExpectedFailure != "":
				xfail++
			default:
				passed++
			}
			total += result.Latency
		}
		mean := time.Duration(0)
		if len(results) > 0 {
			mean = total / time.Duration(len(results))
		}
		fmt.Printf("%s\t%d\t%d\t%d\t%d\t%s\n", model, passed, failed, xfail, xpass, mean.Round(time.Millisecond))
	}
}

func failedCount(results []evalResult) int {
	failed := 0
	for _, result := range results {
		if !result.Passed {
			failed++
		}
	}
	return failed
}

func splitCSV(text string) []string {
	var out []string
	for _, part := range strings.Split(text, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func short(text string) string {
	if len(text) <= 500 {
		return text
	}
	return text[:500] + fmt.Sprintf("... (%d more bytes)", len(text)-500)
}

func valueString(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(data)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}
