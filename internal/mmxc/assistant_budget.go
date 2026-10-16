package mmxc

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The budget_status Ask tool answers budget questions ("how much budget is left
// for groceries this year?", "how much over budget was I last year on hobbies?")
// from the same listBudgetEntries rows the Budgets page shows, so Ask and the
// page cannot disagree. It only reads.

// assistantBudgetMaxEntries bounds the rows sent back to the model when no
// category narrows the question; entry_count still reports the full number.
const assistantBudgetMaxEntries = 40

type assistantBudgetStatusArgs struct {
	Year     string `json:"year"`
	Category string `json:"category"`
}

// assistantBudgetLine is one budget entry as the model sees it. Amounts are
// magnitudes in the budget's own direction: for an expense budget, actual is
// net spending (refunds reduce it); for an income budget, net income received.
// remaining = budget - actual, so it goes negative once the budget is exceeded,
// and over_by is the positive excess (0 while within budget).
type assistantBudgetLine struct {
	Category     string  `json:"category"`
	Kind         string  `json:"kind"`
	Period       string  `json:"period"`
	PeriodAmount float64 `json:"period_amount"`
	Budget       float64 `json:"budget"`
	BudgetToDate float64 `json:"budget_to_date"`
	Actual       float64 `json:"actual"`
	Remaining    float64 `json:"remaining"`
	OverBy       float64 `json:"over_by"`
	UsedPercent  float64 `json:"used_percent"`
}

type assistantBudgetTotals struct {
	Entries      int     `json:"entries"`
	Budget       float64 `json:"budget"`
	BudgetToDate float64 `json:"budget_to_date"`
	Actual       float64 `json:"actual"`
	Remaining    float64 `json:"remaining"`
	OverBy       float64 `json:"over_by"`
}

type assistantBudgetStatus struct {
	Year            string                 `json:"year"`
	Found           bool                   `json:"found"`
	AvailableYears  []string               `json:"available_years,omitempty"`
	Category        string                 `json:"category,omitempty"`
	Expenses        *assistantBudgetTotals `json:"expenses,omitempty"`
	Income          *assistantBudgetTotals `json:"income,omitempty"`
	EntryCount      int                    `json:"entry_count"`
	OverBudgetCount int                    `json:"over_budget_count"`
	Entries         []assistantBudgetLine  `json:"entries"`
	Note            string                 `json:"note,omitempty"`

	budgetID int
}

// BudgetURL links the Ask result to the budget year it was computed from.
func (s *assistantBudgetStatus) BudgetURL() string {
	return budgetRedirect(s.budgetID)
}

// Primary is the totals block the Ask page shows as chips: expenses when the
// result has any, otherwise income.
func (s *assistantBudgetStatus) Primary() *assistantBudgetTotals {
	if s.Expenses != nil {
		return s.Expenses
	}
	return s.Income
}

func (a *app) assistantToolBudgetStatus(args assistantBudgetStatusArgs, today time.Time) (*assistantBudgetStatus, error) {
	yearText := strings.TrimSpace(args.Year)
	if yearText == "" {
		yearText = today.Format("2006")
	}
	year, err := strconv.Atoi(yearText)
	if err != nil || len(yearText) != 4 {
		return nil, fmt.Errorf("budget_status year must be YYYY")
	}
	status := &assistantBudgetStatus{Year: yearText, Entries: []assistantBudgetLine{}}

	var categoryIDs map[int]bool
	if name := strings.TrimSpace(args.Category); name != "" {
		id, err := a.resolveAssistantToolCategoryID(name)
		if err != nil {
			return nil, err
		}
		cats, err := a.listCategories()
		if err != nil {
			return nil, err
		}
		categoryIDs = categoryWithDescendants(cats, id)
		status.Category = categoryPath(cats, id)
	}

	years, err := a.listBudgetYears()
	if err != nil {
		return nil, err
	}
	for _, y := range years {
		if y.Name == yearText {
			status.budgetID = y.ID
		}
		if _, err := strconv.Atoi(y.Name); err == nil && len(y.Name) == 4 {
			status.AvailableYears = append(status.AvailableYears, y.Name)
		}
	}
	if status.budgetID == 0 {
		status.Note = fmt.Sprintf("There is no budget for %s.", yearText)
		return status, nil
	}
	status.Found = true
	status.AvailableYears = nil

	entries, err := a.listBudgetEntries(status.budgetID)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if categoryIDs != nil && !categoryIDs[entry.CategoryID] {
			continue
		}
		line, err := assistantBudgetLineFor(entry, year, today)
		if err != nil {
			return nil, err
		}
		totals := &status.Expenses
		if line.Kind == "income" {
			totals = &status.Income
		} else if line.OverBy > 0 {
			status.OverBudgetCount++
		}
		if *totals == nil {
			*totals = &assistantBudgetTotals{}
		}
		(*totals).Entries++
		(*totals).Budget += line.Budget
		(*totals).BudgetToDate += line.BudgetToDate
		(*totals).Actual += line.Actual
		status.Entries = append(status.Entries, line)
	}
	for _, totals := range []*assistantBudgetTotals{status.Expenses, status.Income} {
		if totals == nil {
			continue
		}
		totals.Budget = roundMoney(totals.Budget)
		totals.BudgetToDate = roundMoney(totals.BudgetToDate)
		totals.Actual = roundMoney(totals.Actual)
		totals.Remaining = roundMoney(totals.Budget - totals.Actual)
		totals.OverBy = roundMoney(math.Max(0, totals.Actual-totals.Budget))
	}
	status.EntryCount = len(status.Entries)
	if status.EntryCount == 0 {
		if status.Category != "" {
			status.Note = fmt.Sprintf("The %s budget has no active entry for %s.", yearText, status.Category)
		} else {
			status.Note = fmt.Sprintf("The %s budget has no active entries.", yearText)
		}
		return status, nil
	}
	// Overruns first (largest excess on top), then expenses by how much of the
	// budget is used, income last, so a truncated list keeps what matters.
	sort.SliceStable(status.Entries, func(i, j int) bool {
		x, y := status.Entries[i], status.Entries[j]
		if x.Kind != y.Kind {
			return x.Kind == "expense"
		}
		if x.OverBy != y.OverBy {
			return x.OverBy > y.OverBy
		}
		if x.UsedPercent != y.UsedPercent {
			return x.UsedPercent > y.UsedPercent
		}
		return x.Category < y.Category
	})
	if len(status.Entries) > assistantBudgetMaxEntries {
		status.Entries = status.Entries[:assistantBudgetMaxEntries]
	}
	return status, nil
}

func assistantBudgetLineFor(entry budgetEntry, year int, today time.Time) (assistantBudgetLine, error) {
	line := assistantBudgetLine{
		Category:     entry.Category,
		Kind:         "expense",
		Period:       entry.Period,
		PeriodAmount: roundMoney(math.Abs(entry.Amount)),
		Budget:       roundMoney(math.Abs(entry.Annual)),
	}
	if entry.Parent != "" {
		line.Category = entry.Parent + ": " + entry.Category
	}
	// MMEX stores expense budgets negative and income budgets positive; actuals
	// are signed the same way (deposits positive).
	actual := -entry.Current
	if entry.Amount > 0 {
		line.Kind = "income"
		actual = entry.Current
	}
	line.Actual = roundMoney(actual)
	switch {
	case year < today.Year():
		line.BudgetToDate = line.Budget
	case year == today.Year():
		toDate, err := budgetAmountForRange(entry.Period, entry.Amount, fmt.Sprintf("%04d-01-01", year), today.Format("2006-01-02"))
		if err != nil {
			return assistantBudgetLine{}, err
		}
		line.BudgetToDate = roundMoney(math.Abs(toDate))
	}
	line.Remaining = roundMoney(line.Budget - line.Actual)
	line.OverBy = roundMoney(math.Max(0, line.Actual-line.Budget))
	if line.Budget > 0 {
		line.UsedPercent = math.Round(line.Actual/line.Budget*1000) / 10
	}
	return line, nil
}

func categoryWithDescendants(cats []category, rootID int) map[int]bool {
	ids := map[int]bool{rootID: true}
	for grew := true; grew; {
		grew = false
		for _, cat := range cats {
			if !ids[cat.ID] && ids[cat.ParentID] {
				ids[cat.ID] = true
				grew = true
			}
		}
	}
	return ids
}

func categoryPath(cats []category, id int) string {
	for _, cat := range cats {
		if cat.ID == id {
			if cat.ParentID > 0 {
				return cat.Parent + ": " + cat.Name
			}
			return cat.Name
		}
	}
	return ""
}

// assistantBudgetResult returns the budget_status result behind an Ask answer,
// or nil when the last tool was something else.
func assistantBudgetResult(result any) *assistantBudgetStatus {
	status, _ := result.(*assistantBudgetStatus)
	return status
}
