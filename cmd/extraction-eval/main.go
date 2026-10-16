// Command extraction-eval measures how well an Ollama model extracts
// transactions from bank/credit-card statements, so the project can decide
// whether the assistant model and the statement-extraction model can be the
// same one or must differ.
//
// It deliberately reuses the app's real prompt and parser
// (mmxc.BuildStatementExtractionRequest / mmxc.ParseStatementExtraction) so the
// eval can never drift from production, and it needs no MMEX database: the
// category list the prompt depends on comes from a catalog file instead.
//
// Workflow (statements come from --statements, never synthesized):
//
//  1. Generate goldens: with no golden JSON yet, run extraction once and write
//     <statement>.expected.json next to a flag to review. (--generate forces a
//     rewrite of existing goldens.)
//  2. The user manually inspects and corrects each golden so it is known-good.
//  3. Score: re-run extraction for each model and compare against the corrected
//     golden, reporting transaction recall/precision and per-field accuracy.
//
// All inputs are external paths; nothing here reads the dev DB or bundles real
// statement data. For local use, --statements defaults to data/samples (gitignored).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/un8s/mmxc/internal/mmxc"
)

// catalog is the external substitute for the MMEX database: the category names
// injected into the extraction prompt (and payees, for reference — the current
// extraction prompt does not inject payees, only categories).
type catalog struct {
	Categories []string `json:"categories"`
	Payees     []string `json:"payees"`
}

// statementJob pairs a discovered statement file with its golden path.
type statementJob struct {
	Path       string
	Name       string
	GoldenPath string
	HasGolden  bool
}

// result holds the outcome of running one model against one statement.
type result struct {
	Model       string
	Statement   string
	Latency     time.Duration
	Prompt      string
	ImageCount  int
	RawResponse string
	Err         error
	Score       scoreReport
}

type countFlag int

func (c *countFlag) String() string   { return strconv.Itoa(int(*c)) }
func (c *countFlag) Set(string) error { *c++; return nil }
func (c *countFlag) IsBoolFlag() bool { return true }

func main() {
	var (
		statementsArg   string
		goldenDir       string
		catalogPath     string
		modelsText      string
		baseURL         string
		apiKey          string
		reasoningEffort string
		timeoutText     string
		temperature     float64
		generate        bool
		amountEps       float64
		promptDir       string
		currentDate     string
		verbose         countFlag
	)
	flag.StringVar(&statementsArg, "statements", "data/samples", "directory of statement files, or a comma-separated list of files")
	flag.StringVar(&goldenDir, "golden", "", "directory for golden JSON files (default: alongside each statement as <name>.expected.json)")
	flag.StringVar(&catalogPath, "catalog", "data/extraction_eval_catalog.json", "JSON file with the categories (and payees) the prompt may use")
	flag.StringVar(&modelsText, "models", "qwen3:30b", "comma-separated model names; image statements need a multimodal model")
	flag.StringVar(&baseURL, "url", "http://localhost:11434/v1", "OpenAI-compatible base URL")
	flag.StringVar(&apiKey, "api-key", os.Getenv("MMXC_LLM_API_KEY"), "bearer token for the endpoint (default $MMXC_LLM_API_KEY)")
	flag.StringVar(&reasoningEffort, "reasoning-effort", "none", "reasoning_effort: none, low, medium, high, or max")
	flag.StringVar(&timeoutText, "timeout", "600s", "per-call timeout")
	flag.Float64Var(&temperature, "temperature", 0, "sampling temperature")
	flag.BoolVar(&generate, "generate", false, "(re)write golden JSON from a single extraction instead of scoring; review and correct the output before scoring")
	flag.Float64Var(&amountEps, "amount-epsilon", 0.01, "amount tolerance when matching transactions")
	flag.StringVar(&promptDir, "prompt-dir", "data/prompts", "directory with a statement.txt override of the built-in prompt guidance, as the app's prompt_dir; empty grades the built-in")
	flag.StringVar(&currentDate, "current-date", time.Now().Format("2006-01-02"), "date standing in for the app's clock, in the prompt and when settling the year of a statement that prints none")
	flag.Var(&verbose, "verbose", "print per-statement detail and failed raw responses; repeat for the rendered prompt and every raw response")
	flag.Parse()

	timeout, err := time.ParseDuration(timeoutText)
	if err != nil {
		fatalf("parse timeout: %v", err)
	}
	cat, err := loadCatalog(catalogPath)
	if err != nil {
		fatalf("load catalog: %v", err)
	}
	jobs, err := discoverStatements(statementsArg, goldenDir)
	if err != nil {
		fatalf("discover statements: %v", err)
	}
	if len(jobs) == 0 {
		fatalf("no statement files found under %q", statementsArg)
	}
	models := splitCSV(modelsText)

	// Reuse the app's own transport so the eval sends exactly the request the
	// app sends, with no second HTTP client to keep in sync.
	caller := &extractionCaller{
		client: mmxc.NewEvalLLMClient(mmxc.LLMSettings{
			BaseURL:         baseURL,
			APIKey:          apiKey,
			ReasoningEffort: reasoningEffort,
			Timeout:         timeout,
		}),
		temperature: temperature,
		prompts:     mmxc.Prompts{Dir: promptDir},
		currentDate: currentDate,
	}
	// Say which guidance is graded, so a score from an override is never
	// mistaken for the built-in prompt's.
	status := caller.prompts.StatementStatus()
	fmt.Printf("statement prompt: %s\n", status.Source())
	if status.Warning != "" {
		fmt.Printf("warning: %s\n", status.Warning)
	}

	if generate {
		runGenerate(caller, jobs, cat, first(models), int(verbose))
		return
	}

	var results []result
	missing := 0
	for _, job := range jobs {
		if !job.HasGolden {
			missing++
			fmt.Printf("SKIP %-40s no golden yet — run with --generate, then correct %s\n", job.Name, relPath(job.GoldenPath))
			continue
		}
		golden, err := readGolden(job.GoldenPath)
		if err != nil {
			fatalf("read golden %s: %v", job.GoldenPath, err)
		}
		for _, model := range models {
			res := runScore(caller, job, cat, model, golden, amountEps)
			results = append(results, res)
			printResult(res, int(verbose))
		}
	}
	printSummary(results)
	if missing > 0 {
		fmt.Printf("\n%d statement(s) had no golden and were skipped. Generate, correct, then re-run.\n", missing)
	}
	if anyErrored(results) {
		os.Exit(1)
	}
}

// runGenerate extracts each statement once with model and writes the golden
// JSON for the user to review and correct.
func runGenerate(caller *extractionCaller, jobs []statementJob, cat catalog, model string, verbose int) {
	if model == "" {
		fatalf("no model given")
	}
	for _, job := range jobs {
		extracted, request, raw, err := extractOne(caller, job, cat, model)
		if err != nil {
			fmt.Printf("FAIL %-40s extraction failed: %v\n", job.Name, err)
			if verbose >= 2 {
				printModelRequest(request)
			}
			if verbose >= 1 {
				printRawResponse(raw)
			}
			continue
		}
		if verbose >= 2 {
			printModelRequest(request)
			printRawResponse(raw)
		}
		if err := writeGolden(job.GoldenPath, extracted); err != nil {
			fatalf("write golden %s: %v", job.GoldenPath, err)
		}
		fmt.Printf("WROTE %-40s %s (%d txns) — review and correct before scoring\n", job.Name, relPath(job.GoldenPath), len(extracted.Transactions))
	}
}

func runScore(caller *extractionCaller, job statementJob, cat catalog, model string, golden mmxc.ExtractedStatement, eps float64) result {
	start := time.Now()
	got, request, raw, err := extractOne(caller, job, cat, model)
	res := result{
		Model:       model,
		Statement:   job.Name,
		Latency:     time.Since(start).Round(time.Millisecond),
		Prompt:      request.Prompt,
		ImageCount:  len(request.Images),
		RawResponse: raw,
	}
	if err != nil {
		res.Err = err
		return res
	}
	res.Score = scoreExtraction(golden, got, eps)
	return res
}

// extractOne builds the prompt the app would use, calls Ollama, and parses the
// response into the normalized comparable shape.
func extractOne(caller *extractionCaller, job statementJob, cat catalog, model string) (mmxc.ExtractedStatement, mmxc.StatementExtractionRequest, string, error) {
	data, err := os.ReadFile(job.Path)
	if err != nil {
		return mmxc.ExtractedStatement{}, mmxc.StatementExtractionRequest{}, "", err
	}
	req, err := mmxc.BuildStatementExtractionRequest(caller.prompts, caller.currentDate, job.Name, data, cat.Categories)
	if err != nil {
		return mmxc.ExtractedStatement{}, req, "", err
	}
	raw, err := caller.generate(model, req.Prompt, req.Images, req.ImageMIMEs)
	if err != nil {
		return mmxc.ExtractedStatement{}, req, raw, err
	}
	parsed, err := mmxc.ParseStatementExtraction(raw, caller.currentDate)
	if err != nil {
		return mmxc.ExtractedStatement{}, req, raw, err
	}
	return parsed, req, raw, nil
}

func discoverStatements(arg, goldenDir string) ([]statementJob, error) {
	var files []string
	if strings.Contains(arg, ",") {
		files = splitCSV(arg)
	} else {
		info, err := os.Stat(arg)
		if err != nil {
			return nil, err
		}
		if info.IsDir() {
			entries, err := os.ReadDir(arg)
			if err != nil {
				return nil, err
			}
			for _, e := range entries {
				if e.IsDir() || !isStatementFile(e.Name()) {
					continue
				}
				files = append(files, filepath.Join(arg, e.Name()))
			}
		} else {
			files = []string{arg}
		}
	}
	sort.Strings(files)
	var jobs []statementJob
	for _, f := range files {
		name := filepath.Base(f)
		goldenPath := goldenPathFor(f, goldenDir)
		_, statErr := os.Stat(goldenPath)
		jobs = append(jobs, statementJob{
			Path:       f,
			Name:       name,
			GoldenPath: goldenPath,
			HasGolden:  statErr == nil,
		})
	}
	return jobs, nil
}

func goldenPathFor(statementPath, goldenDir string) string {
	base := filepath.Base(statementPath)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	goldenName := stem + ".expected.json"
	if goldenDir != "" {
		return filepath.Join(goldenDir, goldenName)
	}
	return filepath.Join(filepath.Dir(statementPath), goldenName)
}

func isStatementFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".pdf", ".png", ".jpg", ".jpeg", ".webp", ".txt":
		return true
	}
	return false
}

func loadCatalog(path string) (catalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return catalog{}, err
	}
	var c catalog
	if err := json.Unmarshal(data, &c); err != nil {
		return catalog{}, err
	}
	return c, nil
}

func readGolden(path string) (mmxc.ExtractedStatement, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return mmxc.ExtractedStatement{}, err
	}
	var s mmxc.ExtractedStatement
	if err := json.Unmarshal(data, &s); err != nil {
		return mmxc.ExtractedStatement{}, err
	}
	return s, nil
}

func writeGolden(path string, s mmxc.ExtractedStatement) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// ----- helpers -----

func splitCSV(text string) []string {
	var out []string
	for _, part := range strings.Split(text, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func first(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

func relPath(path string) string {
	if wd, err := os.Getwd(); err == nil {
		if rel, err := filepath.Rel(wd, path); err == nil && !strings.HasPrefix(rel, "..") {
			return rel
		}
	}
	return path
}

func short(text string) string {
	if len(text) <= 500 {
		return text
	}
	return text[:500] + fmt.Sprintf("... (%d more bytes)", len(text)-500)
}

func anyErrored(results []result) bool {
	for _, r := range results {
		if r.Err != nil {
			return true
		}
	}
	return false
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}

// ----- reporting -----

func printResult(r result, verbose int) {
	printResultTo(os.Stdout, r, verbose)
}

func printResultTo(w io.Writer, r result, verbose int) {
	if r.Err != nil {
		fmt.Fprintf(w, "ERR  %-24s %-32s %s  %v\n", r.Model, r.Statement, r.Latency, r.Err)
		if verbose >= 2 {
			printModelRequestTo(w, r.Prompt, r.ImageCount)
		}
		if verbose >= 1 {
			printRawResponseTo(w, r.RawResponse)
		}
		return
	}
	s := r.Score
	fmt.Fprintf(w, "OK   %-24s %-32s %s  recall=%.0f%% prec=%.0f%% date=%.0f%% type=%.0f%% desc=%.0f%% payee=%.0f%% cat=%.0f%%\n",
		r.Model, r.Statement, r.Latency,
		pct(s.Recall), pct(s.Precision), pct(s.DateAcc), pct(s.TypeAcc), pct(s.DescAcc), pct(s.PayeeAcc), pct(s.CategoryAcc))
	if verbose >= 1 {
		if s.HeaderNotes != "" {
			fmt.Fprintf(w, "     header: %s\n", s.HeaderNotes)
		}
		for _, m := range s.Missed {
			fmt.Fprintf(w, "     missed:  %s\n", m)
		}
		for _, e := range s.Extra {
			fmt.Fprintf(w, "     extra:   %s\n", e)
		}
		for _, m := range s.FieldNotes {
			fmt.Fprintf(w, "     field:   %s\n", m)
		}
	}
	if verbose >= 2 {
		printModelRequestTo(w, r.Prompt, r.ImageCount)
		printRawResponseTo(w, r.RawResponse)
	}
}

func printModelRequest(request mmxc.StatementExtractionRequest) {
	printModelRequestTo(os.Stdout, request.Prompt, len(request.Images))
}

func printModelRequestTo(w io.Writer, prompt string, imageCount int) {
	fmt.Fprintf(w, "----- rendered extraction prompt (%d bytes; images=%d) -----\n",
		len(prompt), imageCount)
	if prompt == "" {
		fmt.Fprintln(w, "<empty>")
	} else {
		fmt.Fprint(w, prompt)
		if !strings.HasSuffix(prompt, "\n") {
			fmt.Fprintln(w)
		}
	}
	if imageCount > 0 {
		fmt.Fprintf(w, "[%d base64 image payload(s) omitted from verbose output]\n", imageCount)
	}
	fmt.Fprintln(w, "----- end rendered extraction prompt -----")
}

func printRawResponse(raw string) {
	printRawResponseTo(os.Stdout, raw)
}

func printRawResponseTo(w io.Writer, raw string) {
	fmt.Fprintf(w, "----- raw model response (%d bytes) -----\n", len(raw))
	if raw == "" {
		fmt.Fprintln(w, "<empty>")
	} else {
		fmt.Fprint(w, raw)
		if !strings.HasSuffix(raw, "\n") {
			fmt.Fprintln(w)
		}
	}
	fmt.Fprintln(w, "----- end raw model response -----")
}

func printSummary(results []result) {
	byModel := map[string][]result{}
	var order []string
	for _, r := range results {
		if _, ok := byModel[r.Model]; !ok {
			order = append(order, r.Model)
		}
		byModel[r.Model] = append(byModel[r.Model], r)
	}
	sort.Strings(order)
	fmt.Println()
	fmt.Println("Model\tStmts\tErr\tRecall\tPrec\tDate\tType\tDesc\tPayee\tCat\tMeanLat")
	for _, model := range order {
		rs := byModel[model]
		var agg scoreReport
		var lat time.Duration
		scored, errs := 0, 0
		for _, r := range rs {
			lat += r.Latency
			if r.Err != nil {
				errs++
				continue
			}
			scored++
			agg.Recall += r.Score.Recall
			agg.Precision += r.Score.Precision
			agg.DateAcc += r.Score.DateAcc
			agg.TypeAcc += r.Score.TypeAcc
			agg.DescAcc += r.Score.DescAcc
			agg.PayeeAcc += r.Score.PayeeAcc
			agg.CategoryAcc += r.Score.CategoryAcc
		}
		n := float64(scored)
		if n == 0 {
			n = 1
		}
		meanLat := time.Duration(0)
		if len(rs) > 0 {
			meanLat = lat / time.Duration(len(rs))
		}
		fmt.Printf("%s\t%d\t%d\t%.0f%%\t%.0f%%\t%.0f%%\t%.0f%%\t%.0f%%\t%.0f%%\t%.0f%%\t%s\n",
			model, scored, errs,
			pct(agg.Recall/n), pct(agg.Precision/n), pct(agg.DateAcc/n), pct(agg.TypeAcc/n),
			pct(agg.DescAcc/n), pct(agg.PayeeAcc/n), pct(agg.CategoryAcc/n), meanLat.Round(time.Millisecond))
	}
}

func pct(fraction float64) float64 { return math.Round(fraction*1000) / 10 }

// extractionCaller binds the shared transport to the eval's sampling settings.
type extractionCaller struct {
	client      *mmxc.EvalLLMClient
	temperature float64
	prompts     mmxc.Prompts
	currentDate string
}

func (c *extractionCaller) generate(model, prompt string, images, mimes []string) (string, error) {
	res, err := c.client.Generate(model, prompt, images, mimes, true, c.temperature)
	return res.Content, err
}
