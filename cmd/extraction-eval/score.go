package main

import (
	"fmt"
	"math"
	"strings"

	"github.com/un8s/mmxc/internal/mmxc"
)

// scoreReport captures how a model's extraction compared to the golden.
// Recall/Precision are over transaction *rows*; the per-field accuracies are
// over the matched pairs only (a field can only be judged where there is a row
// to judge it against).
type scoreReport struct {
	GoldenCount int
	GotCount    int
	Matched     int
	Recall      float64 // matched / golden rows  (did it find them all?)
	Precision   float64 // matched / got rows     (did it invent rows?)
	DateAcc     float64 // among matched pairs
	TypeAcc     float64
	DescAcc     float64
	PayeeAcc    float64
	CategoryAcc float64

	Missed      []string // golden rows with no match
	Extra       []string // got rows with no match
	FieldNotes  []string // per-field mismatches on matched pairs
	HeaderNotes string   // period / closing / account mismatches
}

// scoreExtraction matches got transactions to golden ones (primarily by amount
// and type, the financially-identifying fields, then by date), then measures
// field-level accuracy over the matched pairs. Matching is greedy and tolerant
// because the whole point is to grade an imperfect model, not to require byte
// equality.
func scoreExtraction(golden, got mmxc.ExtractedStatement, eps float64) scoreReport {
	goldenTransactions := scoredTransactions(golden)
	gotTransactions := scoredTransactions(got)
	rep := scoreReport{GoldenCount: len(goldenTransactions), GotCount: len(gotTransactions)}

	usedGot := make([]bool, len(gotTransactions))
	var dateHits, typeHits, descHits, payeeHits, catHits float64

	for _, g := range goldenTransactions {
		j := bestMatch(g, gotTransactions, usedGot, eps)
		if j < 0 {
			rep.Missed = append(rep.Missed, describeTxn(g))
			continue
		}
		usedGot[j] = true
		rep.Matched++
		m := gotTransactions[j]

		if g.Date == m.Date {
			dateHits++
		} else {
			rep.FieldNotes = append(rep.FieldNotes, fmt.Sprintf("date %q: got %q want %q", describeTxn(g), m.Date, g.Date))
		}
		if normType(g.Type) == normType(m.Type) {
			typeHits++
		} else {
			rep.FieldNotes = append(rep.FieldNotes, fmt.Sprintf("type %q: got %q want %q", describeTxn(g), m.Type, g.Type))
		}
		if descMatch(g.Description, m.Description) {
			descHits++
		} else {
			rep.FieldNotes = append(rep.FieldNotes, fmt.Sprintf("desc %q: got %q", describeTxn(g), m.Description))
		}
		// Payee/category accuracy is judged only where the golden has a value;
		// a blank golden field is not a target. Where golden is blank, count it
		// as a hit so the metric reflects "got the ones that mattered".
		if normText(g.Payee) == "" || normText(g.Payee) == normText(m.Payee) {
			payeeHits++
		} else {
			rep.FieldNotes = append(rep.FieldNotes, fmt.Sprintf("payee %q: got %q want %q", describeTxn(g), m.Payee, g.Payee))
		}
		if normText(g.Category) == "" || normText(g.Category) == normText(m.Category) {
			catHits++
		} else {
			rep.FieldNotes = append(rep.FieldNotes, fmt.Sprintf("category %q: got %q want %q", describeTxn(g), m.Category, g.Category))
		}
	}
	for j, used := range usedGot {
		if !used {
			rep.Extra = append(rep.Extra, describeTxn(gotTransactions[j]))
		}
	}

	rep.Recall = ratio(rep.Matched, rep.GoldenCount)
	rep.Precision = ratio(rep.Matched, rep.GotCount)
	matched := float64(rep.Matched)
	rep.DateAcc = safeDiv(dateHits, matched)
	rep.TypeAcc = safeDiv(typeHits, matched)
	rep.DescAcc = safeDiv(descHits, matched)
	rep.PayeeAcc = safeDiv(payeeHits, matched)
	rep.CategoryAcc = safeDiv(catHits, matched)
	rep.HeaderNotes = headerNotes(golden, got, eps)
	return rep
}

// bestMatch finds the index of the unused got transaction that best matches g.
// It prefers an exact (date+type+amount) match, then type+amount, then amount
// alone, all within the amount tolerance.
func bestMatch(g mmxc.ExtractedTransaction, got []mmxc.ExtractedTransaction, used []bool, eps float64) int {
	best, bestRank := -1, -1
	for j := range got {
		if used[j] {
			continue
		}
		m := got[j]
		if g.AccountKey != "" && m.AccountKey != "" && g.AccountKey != m.AccountKey {
			continue
		}
		if !amountClose(g.Amount, m.Amount, eps) {
			continue
		}
		rank := 1 // amount only
		if normType(g.Type) == normType(m.Type) {
			rank = 2 // amount + type
			if g.Date == m.Date {
				rank = 3 // amount + type + date
			}
		}
		if rank > bestRank {
			best, bestRank = j, rank
			if rank == 3 {
				break
			}
		}
	}
	return best
}

func headerNotes(golden, got mmxc.ExtractedStatement, eps float64) string {
	goldenAccounts := extractedAccounts(golden)
	gotAccounts := extractedAccounts(got)
	if len(goldenAccounts) > 1 || len(gotAccounts) > 1 {
		var notes []string
		if len(goldenAccounts) != len(gotAccounts) {
			notes = append(notes, fmt.Sprintf("account sections got %d want %d", len(gotAccounts), len(goldenAccounts)))
		}
		used := make([]bool, len(gotAccounts))
		for _, expected := range goldenAccounts {
			match := -1
			for i, actual := range gotAccounts {
				if used[i] {
					continue
				}
				if expected.AccountNumber != "" && normAccount(expected.AccountNumber) == normAccount(actual.AccountNumber) {
					match = i
					break
				}
				if expected.AccountNumber == "" && normText(expected.AccountName) == normText(actual.AccountName) {
					match = i
					break
				}
			}
			if match < 0 {
				notes = append(notes, fmt.Sprintf("account section %q missing", expected.AccountNumber))
				continue
			}
			used[match] = true
			if note := accountHeaderNotes(expected, gotAccounts[match], eps); note != "" {
				notes = append(notes, expected.AccountNumber+": "+note)
			}
		}
		return strings.Join(notes, "; ")
	}
	return accountHeaderNotes(goldenAccounts[0], gotAccounts[0], eps)
}

func accountHeaderNotes(golden, got mmxc.ExtractedAccount, eps float64) string {
	var notes []string
	if golden.PeriodStart != "" && golden.PeriodStart != got.PeriodStart {
		notes = append(notes, fmt.Sprintf("period start got %q want %q", got.PeriodStart, golden.PeriodStart))
	}
	if golden.PeriodEnd != "" && golden.PeriodEnd != got.PeriodEnd {
		notes = append(notes, fmt.Sprintf("period end got %q want %q", got.PeriodEnd, golden.PeriodEnd))
	}
	if golden.HasClosing {
		if !got.HasClosing {
			notes = append(notes, "closing balance missing")
		} else if !amountClose(golden.ClosingBalance, got.ClosingBalance, eps) {
			notes = append(notes, fmt.Sprintf("closing balance got %.2f want %.2f", got.ClosingBalance, golden.ClosingBalance))
		}
	}
	if golden.AccountNumber != "" && normAccount(golden.AccountNumber) != normAccount(got.AccountNumber) {
		notes = append(notes, fmt.Sprintf("account number got %q want %q", got.AccountNumber, golden.AccountNumber))
	}
	return strings.Join(notes, "; ")
}

func extractedAccounts(statement mmxc.ExtractedStatement) []mmxc.ExtractedAccount {
	if len(statement.Accounts) > 0 {
		return statement.Accounts
	}
	return []mmxc.ExtractedAccount{{
		AccountName: statement.AccountName, AccountNumber: statement.AccountNumber,
		PeriodStart: statement.PeriodStart, PeriodEnd: statement.PeriodEnd,
		ClosingDate: statement.ClosingDate, ClosingBalance: statement.ClosingBalance,
		HasClosing: statement.HasClosing, Transactions: statement.Transactions,
	}}
}

func scoredTransactions(statement mmxc.ExtractedStatement) []mmxc.ExtractedTransaction {
	var out []mmxc.ExtractedTransaction
	for _, account := range extractedAccounts(statement) {
		accountKey := ""
		if account.AccountNumber != "" {
			accountKey = "number:" + normAccount(account.AccountNumber)
		} else if account.AccountName != "" {
			accountKey = "name:" + normText(account.AccountName)
		}
		for _, transaction := range account.Transactions {
			transaction.AccountKey = accountKey
			out = append(out, transaction)
		}
	}
	return out
}

func describeTxn(t mmxc.ExtractedTransaction) string {
	desc := t.Description
	if len(desc) > 32 {
		desc = desc[:32]
	}
	return fmt.Sprintf("%s %s %.2f %s", t.Date, normType(t.Type), t.Amount, desc)
}

func amountClose(a, b, eps float64) bool { return math.Abs(a-b) <= eps }

// descMatch tolerates the model cleaning/trimming a description: a hit is exact
// (normalized) equality or one string containing the other.
func descMatch(golden, got string) bool {
	g, m := normText(golden), normText(got)
	if g == "" {
		return true
	}
	if m == "" {
		return false
	}
	return g == m || strings.Contains(g, m) || strings.Contains(m, g)
}

func normText(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

func normType(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "withdrawal", "debit", "withdraw":
		return "withdrawal"
	case "deposit", "credit":
		return "deposit"
	case "transfer":
		return "transfer"
	}
	return s
}

// normAccount keeps only digits so masked variants (****1234 vs 1234) compare
// equal on their visible digits.
func normAccount(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func ratio(num, den int) float64 {
	if den == 0 {
		return 0
	}
	return float64(num) / float64(den)
}

func safeDiv(num, den float64) float64 {
	if den == 0 {
		return 0
	}
	return num / den
}
