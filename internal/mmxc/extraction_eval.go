package mmxc

import (
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"
)

// This file exposes the database-independent pieces of statement extraction so
// the extraction eval (cmd/extraction-eval) can exercise the *exact* prompt and
// parser the app uses, without a real MMEX database. Keeping a single source for
// the prompt means the eval can never drift from production the way a copied
// prompt template would: the eval reads the same built-in guidance, or the same
// prompt_dir override, as the app. The only DB-derived input to extraction is
// the category list, which the eval supplies from its catalog file instead.

// StatementExtractionRequest is the model input the app would send for an
// uploaded statement: the rendered prompt and any base64-encoded images.
type StatementExtractionRequest struct {
	Prompt string
	Images []string
	// ImageMIMEs parallels Images: the OpenAI content-part shape carries each
	// image as a data: URI, which needs the type as well as the bytes.
	ImageMIMEs []string
}

// BuildStatementExtractionRequest mirrors the input stage of
// extractCandidatesFromStatement without a database: it decides the prompt and
// images from the file type and injects the supplied category list (the app's
// only DB-derived prompt input). prompts names the override directory the
// guidance is read from, exactly as the app's prompt_dir setting does, and
// currentDate (YYYY-MM-DD) stands in for the app's clock. PDFs are reduced to
// text with the same pdftotext invocation the app uses; image files are
// returned as images, which need a multimodal model.
func BuildStatementExtractionRequest(prompts Prompts, currentDate, filename string, data []byte, categories []string) (StatementExtractionRequest, error) {
	ext := strings.ToLower(filepath.Ext(filename))
	switch {
	case ext == ".png" || ext == ".jpg" || ext == ".jpeg" || ext == ".webp":
		return StatementExtractionRequest{
			Prompt:     statementImagePrompt(prompts, currentDate, categories),
			Images:     []string{base64.StdEncoding.EncodeToString(data)},
			ImageMIMEs: []string{imageMIME(ext)},
		}, nil
	case ext == ".pdf":
		text, err := extractPDFTextResult(data)
		if err != nil {
			return StatementExtractionRequest{}, err
		}
		if len(strings.Fields(text)) < 20 {
			return StatementExtractionRequest{}, errors.New("could not extract enough text from PDF; upload an image-capable statement screenshot instead")
		}
		return StatementExtractionRequest{Prompt: statementTextPrompt(prompts, currentDate, text, categories)}, nil
	default:
		text := strings.TrimSpace(string(data))
		if len(strings.Fields(text)) < 10 {
			return StatementExtractionRequest{}, errors.New("statement text is too short for extraction")
		}
		return StatementExtractionRequest{Prompt: statementTextPrompt(prompts, currentDate, text, categories)}, nil
	}
}

// ExtractedStatement is the normalized, comparable result of parsing a model
// response — the shape the extraction eval stores as its golden JSON and scores
// against. It mirrors the statement header plus the transaction fields the model
// is asked to produce, omitting the app-internal resolution state.
type ExtractedStatement struct {
	Accounts       []ExtractedAccount     `json:"accounts,omitempty"`
	AccountName    string                 `json:"account_name,omitempty"`
	AccountNumber  string                 `json:"account_number,omitempty"`
	PeriodStart    string                 `json:"period_start,omitempty"`
	PeriodEnd      string                 `json:"period_end,omitempty"`
	ClosingDate    string                 `json:"closing_date,omitempty"`
	ClosingBalance float64                `json:"closing_balance,omitempty"`
	HasClosing     bool                   `json:"has_closing,omitempty"`
	Transactions   []ExtractedTransaction `json:"transactions"`
}

// ExtractedAccount is one independently scored account section in a
// multi-account statement. ExtractedStatement's legacy flat fields remain for
// existing single-account goldens.
type ExtractedAccount struct {
	AccountName    string                 `json:"account_name,omitempty"`
	AccountNumber  string                 `json:"account_number,omitempty"`
	PeriodStart    string                 `json:"period_start,omitempty"`
	PeriodEnd      string                 `json:"period_end,omitempty"`
	ClosingDate    string                 `json:"closing_date,omitempty"`
	ClosingBalance float64                `json:"closing_balance,omitempty"`
	HasClosing     bool                   `json:"has_closing,omitempty"`
	Transactions   []ExtractedTransaction `json:"transactions"`
}

// ExtractedTransaction is one row of an ExtractedStatement.
type ExtractedTransaction struct {
	Date        string  `json:"date"`
	Type        string  `json:"type"`
	Amount      float64 `json:"amount"`
	Description string  `json:"description"`
	Payee       string  `json:"payee,omitempty"`
	Category    string  `json:"category,omitempty"`
	// AccountKey is populated only by the eval scorer while flattening a
	// multi-account result, so otherwise-identical rows cannot match across
	// account sections. It is not part of the golden JSON.
	AccountKey string `json:"-"`
}

// ParseStatementExtraction parses a raw model response exactly as the app does
// (parseStatementSectionsResponse, then settleStatementYears with currentDate
// standing in for the app's clock) and returns the normalized result for eval
// comparison.
func ParseStatementExtraction(raw, currentDate string) (ExtractedStatement, error) {
	sections, _, err := parseStatementSectionsResponse(raw)
	if err != nil {
		return ExtractedStatement{}, err
	}
	settleStatementYears(sections, currentDate)
	accounts := make([]ExtractedAccount, 0, len(sections))
	for _, section := range sections {
		account := ExtractedAccount{
			AccountName:    section.Header.AccountName,
			AccountNumber:  section.Header.AccountNumber,
			PeriodStart:    section.Header.PeriodStart,
			PeriodEnd:      section.Header.PeriodEnd,
			ClosingDate:    section.Header.ClosingDate,
			ClosingBalance: section.Header.ClosingBalance,
			HasClosing:     section.Header.HasClosing,
			Transactions:   []ExtractedTransaction{},
		}
		for _, c := range section.Candidates {
			account.Transactions = append(account.Transactions, ExtractedTransaction{
				Date: c.Date, Type: c.Type, Amount: c.Amount, Description: c.Description,
				Payee: c.Payee, Category: c.Category,
			})
		}
		accounts = append(accounts, account)
	}
	if len(accounts) == 1 {
		account := accounts[0]
		return ExtractedStatement{
			AccountName: account.AccountName, AccountNumber: account.AccountNumber,
			PeriodStart: account.PeriodStart, PeriodEnd: account.PeriodEnd,
			ClosingDate: account.ClosingDate, ClosingBalance: account.ClosingBalance,
			HasClosing: account.HasClosing, Transactions: account.Transactions,
		}, nil
	}
	return ExtractedStatement{Accounts: accounts, Transactions: []ExtractedTransaction{}}, nil
}
