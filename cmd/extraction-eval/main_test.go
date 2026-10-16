package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/un8s/mmxc/internal/mmxc"
)

// loadFixtureGolden reads the committed synthetic golden used across tests.
func loadFixtureGolden(t *testing.T) mmxc.ExtractedStatement {
	t.Helper()
	golden, err := readGolden(filepath.Join("testdata", "statement.expected.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if len(golden.Transactions) != 5 {
		t.Fatalf("golden should have 5 transactions, got %d", len(golden.Transactions))
	}
	return golden
}

func TestScorePerfectMatch(t *testing.T) {
	golden := loadFixtureGolden(t)
	rep := scoreExtraction(golden, golden, 0.01)
	if rep.Recall != 1 || rep.Precision != 1 {
		t.Fatalf("perfect match should be recall/precision 1, got %.2f/%.2f", rep.Recall, rep.Precision)
	}
	for name, v := range map[string]float64{
		"date": rep.DateAcc, "type": rep.TypeAcc, "desc": rep.DescAcc,
		"payee": rep.PayeeAcc, "category": rep.CategoryAcc,
	} {
		if v != 1 {
			t.Errorf("%s accuracy should be 1, got %.2f", name, v)
		}
	}
	if rep.HeaderNotes != "" {
		t.Errorf("header notes should be empty, got %q", rep.HeaderNotes)
	}
}

func TestScoreMissedAndExtra(t *testing.T) {
	golden := loadFixtureGolden(t)
	got := mmxc.ExtractedStatement{
		// Drop one golden row (the interest deposit) and invent one.
		Transactions: append([]mmxc.ExtractedTransaction{}, golden.Transactions[:4]...),
	}
	got.Transactions = append(got.Transactions, mmxc.ExtractedTransaction{
		Date: "2025-03-20", Type: "Withdrawal", Amount: 999.99, Description: "HALLUCINATED ROW",
	})
	rep := scoreExtraction(golden, got, 0.01)
	if rep.Matched != 4 {
		t.Fatalf("expected 4 matched, got %d", rep.Matched)
	}
	if len(rep.Missed) != 1 || len(rep.Extra) != 1 {
		t.Fatalf("expected 1 missed and 1 extra, got %d/%d", len(rep.Missed), len(rep.Extra))
	}
	if rep.Recall != 0.8 {
		t.Errorf("recall should be 0.8, got %.3f", rep.Recall)
	}
	if rep.Precision != 0.8 {
		t.Errorf("precision should be 0.8, got %.3f", rep.Precision)
	}
}

func TestScoreFieldMismatches(t *testing.T) {
	golden := loadFixtureGolden(t)
	got := mmxc.ExtractedStatement{Transactions: make([]mmxc.ExtractedTransaction, len(golden.Transactions))}
	copy(got.Transactions, golden.Transactions)
	// Same amounts/types (so all rows still match) but wrong category and date
	// on one row, wrong payee on another.
	got.Transactions[0].Category = "Food: Dining Out"
	got.Transactions[0].Date = "2025-03-04"
	got.Transactions[1].Payee = "Wrong Employer"
	rep := scoreExtraction(golden, got, 0.01)
	if rep.Matched != 5 {
		t.Fatalf("all rows should still match by amount+type, got %d", rep.Matched)
	}
	if rep.CategoryAcc != 0.8 {
		t.Errorf("category accuracy should be 0.8, got %.3f", rep.CategoryAcc)
	}
	if rep.DateAcc != 0.8 {
		t.Errorf("date accuracy should be 0.8, got %.3f", rep.DateAcc)
	}
	if rep.PayeeAcc != 0.8 {
		t.Errorf("payee accuracy should be 0.8, got %.3f", rep.PayeeAcc)
	}
}

func TestDescMatchTolerance(t *testing.T) {
	if !descMatch("POS PURCHASE FERN CAFE", "Fern Cafe") {
		t.Error("contained cleaned description should match")
	}
	if descMatch("POS PURCHASE FERN CAFE", "GREENLEAF GROCERS") {
		t.Error("unrelated descriptions should not match")
	}
}

func TestNormType(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"Withdrawal", "withdrawal"}, {"debit", "withdrawal"},
		{"Deposit", "deposit"}, {"credit", "deposit"},
	} {
		if got := normType(c.in); got != c.want {
			t.Errorf("normType(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestBuildPromptUsesAppPipeline confirms the eval drives the real app prompt
// builder: a plain-text statement plus the catalog categories must appear in the
// rendered prompt, with no DB involved.
func TestBuildPromptUsesAppPipeline(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "statement.txt"))
	if err != nil {
		t.Fatalf("read statement: %v", err)
	}
	cat, err := loadCatalog(filepath.Join("testdata", "catalog.json"))
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	req, err := mmxc.BuildStatementExtractionRequest(mmxc.Prompts{}, "2026-06-19", "statement.txt", data, cat.Categories)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if len(req.Images) != 0 {
		t.Error("text statement should not carry images")
	}
	for _, want := range []string{"GREENLEAF GROCERS", "Food: Groceries", "Housing: Mortgage", `"accounts"`} {
		if !strings.Contains(req.Prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestParseStatementExtraction(t *testing.T) {
	raw := `{"account":{"name":"Everyday Chequing Account","number":"****4821"},
	"period":{"start":"2025-03-01","end":"2025-03-31"},
	"closing_date":"2025-03-31","closing_balance":2354.10,
	"transactions":[{"date":"2025-03-03","type":"Withdrawal","amount":84.20,"description":"POS PURCHASE GREENLEAF GROCERS","payee":"Greenleaf Grocers","category":"Food: Groceries"}]}`
	parsed, err := mmxc.ParseStatementExtraction(raw, "2026-06-19")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed.Transactions) != 1 || parsed.Transactions[0].Amount != 84.20 {
		t.Fatalf("unexpected parse result: %+v", parsed)
	}
	if !parsed.HasClosing || parsed.ClosingBalance != 2354.10 {
		t.Errorf("closing balance not parsed: %+v", parsed)
	}
}

func TestParseMultiAccountExtractionAndScoreEmptySection(t *testing.T) {
	raw := `{"accounts":[
		{"account":{"name":"Everyday Savings","number":"****1111"},"period":{"start":"2025-03-01","end":"2025-03-31"},"closing_date":"2025-03-31","closing_balance":525,"transactions":[{"date":"2025-03-15","type":"Deposit","amount":25,"description":"INTEREST PAID","payee":"Northwind Savings Bank","category":"Income: Interest"}]},
		{"account":{"name":"Future Fund","number":"****2222"},"period":{"start":"2025-03-01","end":"2025-03-31"},"closing_date":"2025-03-31","closing_balance":0.01,"transactions":[]}
	]}`
	parsed, err := mmxc.ParseStatementExtraction(raw, "2026-06-19")
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Accounts) != 2 || len(parsed.Accounts[1].Transactions) != 0 || !parsed.Accounts[1].HasClosing {
		t.Fatalf("multi-account parse = %+v", parsed)
	}
	golden, err := readGolden(filepath.Join("testdata", "multi-account.expected.json"))
	if err != nil {
		t.Fatal(err)
	}
	report := scoreExtraction(golden, parsed, 0.01)
	if report.Recall != 1 || report.Precision != 1 || report.HeaderNotes != "" {
		t.Fatalf("multi-account score = %+v", report)
	}
}

func TestGoldenRoundTrip(t *testing.T) {
	golden := loadFixtureGolden(t)
	path := filepath.Join(t.TempDir(), "out.expected.json")
	if err := writeGolden(path, golden); err != nil {
		t.Fatalf("write: %v", err)
	}
	back, err := readGolden(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(back.Transactions) != len(golden.Transactions) {
		t.Fatalf("round-trip lost transactions: %d vs %d", len(back.Transactions), len(golden.Transactions))
	}
}

func TestDiscoverStatements(t *testing.T) {
	jobs, err := discoverStatements("testdata", "")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	var found *statementJob
	for i := range jobs {
		if jobs[i].Name == "statement.txt" {
			found = &jobs[i]
		}
	}
	if found == nil {
		t.Fatal("statement.txt not discovered")
	}
	if !found.HasGolden {
		t.Error("statement.txt should have its committed golden detected")
	}
}

func TestVerbosePrintsCompleteRawResponseOnExtractionError(t *testing.T) {
	raw := "The model returned prose instead of JSON.\nSecond line."
	prompt := "Extract this statement:\nPDF TRANSACTION TEXT"
	res := result{
		Model:       "synthetic-model",
		Statement:   "statement.txt",
		Prompt:      prompt,
		RawResponse: raw,
		Err:         errors.New("Ollama response did not contain a JSON array"),
	}
	var output strings.Builder
	printResultTo(&output, res, 1)
	got := output.String()
	if !strings.Contains(got, raw) {
		t.Fatalf("verbose error output omitted raw response:\n%s", got)
	}
	if !strings.Contains(got, "raw model response ("+strconv.Itoa(len(raw))+" bytes)") {
		t.Fatalf("verbose error output omitted response boundary:\n%s", got)
	}
	if strings.Contains(got, prompt) {
		t.Fatalf("single verbose output unexpectedly included rendered prompt:\n%s", got)
	}

	output.Reset()
	printResultTo(&output, res, 2)
	got = output.String()
	if !strings.Contains(got, prompt) {
		t.Fatalf("double verbose output omitted rendered prompt:\n%s", got)
	}
	if !strings.Contains(got, "rendered extraction prompt ("+strconv.Itoa(len(prompt))+" bytes; images=0)") {
		t.Fatalf("double verbose output omitted request metadata:\n%s", got)
	}
	if !strings.Contains(got, raw) {
		t.Fatalf("double verbose output omitted raw response:\n%s", got)
	}

	output.Reset()
	printResultTo(&output, res, 0)
	if strings.Contains(output.String(), raw) {
		t.Fatalf("normal error output included raw response:\n%s", output.String())
	}
}
