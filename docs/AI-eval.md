# AI evals

MMXC has two evals that grade the models the app relies on. Both talk to the
same OpenAI-compatible endpoint the app uses (Ollama, OpenRouter, OpenAI, vLLM, …):

- **Assistant eval** (`cmd/assistant-eval`) — does the model pick the right read-only
  tool, with exact filters and the right clarification behavior?
- **Statement-extraction eval** (`cmd/extraction-eval`) — how accurately does the model
  turn a bank/credit-card statement into transactions?

Together they answer the practical question: **can one model serve both the assistant and
statement extraction, or do you need different models?** Run both against the candidate
model(s) and compare. Small local models differ a lot here, so it is worth checking before
you rely on one.

## Shared principles

- **No prompt/parse copies to keep in sync.** Both evals call the app's real code, so they
  grade exactly what production does and can never drift:
  - assistant: `mmxc.BuildAssistantToolPrompt` + `mmxc.ParseAssistantToolDecision`
  - extraction: `mmxc.BuildStatementExtractionRequest` + `mmxc.ParseStatementExtraction`
- **Custom prompts are graded too.** Both read prompt overrides from `--prompt-dir` (default
  `data/prompts`, the app's default `prompt_dir`) and print whether they grade the built-in
  guidance or your file. Pass `--prompt-dir ""` to grade the built-in. See
  [AI-prompts.md](AI-prompts.md).
- **No database.** The only DB-derived prompt inputs are injected from files/flags instead:
  account names (`--accounts`) for the assistant, the category list (`--catalog`) for
  extraction.
- **Committed fixtures are synthetic; real data stays local.** `go test` runs against
  invented fixtures (`docs/assistant_eval_cases.json`, `cmd/extraction-eval/testdata/`).
  Your own statements, their goldens, and your category catalog belong under `data/`
  (gitignored) and must never be committed.
- **Offline by default.** The Go tests never call a model endpoint. A model is only
  contacted when you run the eval binaries / `just` recipes below.

Both default to `--url http://localhost:11434/v1` (note the `/v1`); set `MMXC_LLM_BASE_URL`
to point at a LAN Ollama box or a hosted provider, and `--api-key` (default
`$MMXC_LLM_API_KEY`) when the endpoint requires one. Pass several models as a comma list
to compare them side by side.

Both reuse the app's own HTTP transport via `mmxc.NewEvalLLMClient`, so there is no second
client to keep in sync — the same rule that already applies to the prompts and parsers.

> The OpenAI API has no per-request context-window parameter. Set the window on the server
> instead (`OLLAMA_CONTEXT_LENGTH=32768 ollama serve`); the client detects and reports silent
> prompt truncation.

---

## Assistant eval

Measures the assistant's tool-selection decision for a question (optionally mid-conversation
via a transcript): the chosen action/tool, the filter arguments, and whether it asks for
clarification when a lookup is ambiguous.

### Run it

```sh
just assistant-eval
# or directly:
go run ./cmd/assistant-eval \
  --url http://localhost:11434/v1 \
  --models qwen3:30b,qwen3:14b \
  --cases docs/assistant_eval_cases.json \
  --verbose
```

Env vars used by the recipe: `MMXC_LLM_BASE_URL`, `MMXC_LLM_API_KEY`, `MMXC_ASSISTANT_EVAL_MODELS`.

Key flags (`go run ./cmd/assistant-eval --help` for all):

| Flag | Default | Purpose |
|------|---------|---------|
| `--models` | `qwen3:30b` | comma-separated models to grade |
| `--cases` | `docs/assistant_eval_cases.json` | the eval cases |
| `--accounts` | `Primary Checking, Rewards Card, Shared Savings` | account names injected into the prompt (stands in for the DB) |
| `--current-date` | today | date injected into the prompt (drives relative-period scoring) |
| `--reasoning-effort` | `none` | `none`, `low`, `medium`, `high`, or `max` |
| `--prompt-dir` | `data/prompts` | where an `assistant.txt` override is read from; `""` grades the built-in |
| `--api-key` | `$MMXC_LLM_API_KEY` | bearer token for the endpoint |
| `--temperature` | `0` | sampling temperature |
| `--timeout` | `120s` | per-call timeout |
| `--verbose` | off | per-case detail; repeat (`-verbose -verbose`) for the rendered prompt and raw response |

### Case format (`docs/assistant_eval_cases.json`)

A JSON array of cases. Each case:

```json
{
  "name": "spending-last-month",
  "question": "How much did I spend last month?",
  "transcript": [],
  "expect": {
    "action": "tool",
    "tool": "spending_summary",
    "arguments": { "from": "{{last_month_start}}", "to": "{{last_month_end}}", "type": "Withdrawal" },
    "absent": ["account"],
    "answer_contains": []
  }
}
```

- `expect` is the required decision. `arguments` must be present and equal; `absent` lists
  arguments that must be missing/empty (e.g. dates the model should not have invented);
  tool calls must leave `answer` empty.
- `any_of: [ … ]` instead of `expect` passes when any one expectation matches (e.g. two
  acceptable date ranges).
- An argument value can itself be `{"any_of": [a, b]}` to accept alternatives for one field.
- Date placeholders like `{{last_month_start}}`, `{{current_year}}`, `{{current_date}}` are
  resolved against `--current-date`, so cases stay valid as time passes.
- `expected_failure: "reason"` marks a known weakness: it reports `XFAIL` when it still
  fails and `XPASS` when it unexpectedly passes, without breaking the run.

**Keep cases synthetic** — invented accounts, payees and categories only, never anything
from a real database or statement.

### Scoring & output

Per case: `action`/`tool` must match, expected `arguments` must be present and equal,
`absent` arguments must be empty, `answer_contains` substrings must appear, and tool calls
must have an empty `answer`. The summary prints pass/fail/XFAIL/XPASS counts and mean latency
per model. The process exits non-zero if any case fails.

When you change the assistant tool prompt in `internal/mmxc/assistant.go`, you do **not**
edit a prompt copy (the eval reuses the builder) — just add or adjust cases in
`docs/assistant_eval_cases.json` and re-run.

---

## Statement-extraction eval

Grades how well a model extracts an `ExtractedStatement` (one or more account sections plus
transaction rows) from a statement file. Because correcting a model's output by hand is the
only way to get ground truth, it is a multi-step workflow: provide a catalog, add statements,
generate goldens, correct them, then score.

To try the harness without any real data, point it at the synthetic fixtures:

```sh
go run ./cmd/extraction-eval \
  --statements cmd/extraction-eval/testdata \
  --catalog cmd/extraction-eval/testdata/catalog.json
```

### 1. Provide a catalog (categories the prompt may use)

Extraction injects your existing categories so the model reuses them. Supply them in a
JSON file (payees are accepted for reference but the current extraction prompt injects only
categories). `cmd/extraction-eval/testdata/catalog.json` is a small example:

```json
{
  "categories": ["Food: Groceries", "Housing: Mortgage", "Income: Salary"],
  "payees": ["Greenleaf Grocers", "Contoso Ltd"]
}
```

To build one from your own database (keep the output under the gitignored `data/`):

```sh
python3 - <<'PY'
import sqlite3, json
db = sqlite3.connect("file:path/to/your.mmb?mode=ro", uri=True)
cats = [f"{p}: {c}" for p, c in db.execute("""
  SELECT par.CATEGNAME, ch.CATEGNAME FROM CATEGORY_V1 ch
  JOIN CATEGORY_V1 par ON ch.PARENTID = par.CATEGID
  WHERE ch.PARENTID > 0 AND (ch.ACTIVE IS NULL OR ch.ACTIVE <> 0)
  ORDER BY 1, 2 COLLATE NOCASE""")]
payees = [r[0] for r in db.execute(
  "SELECT PAYEENAME FROM PAYEE_V1 WHERE ACTIVE IS NULL OR ACTIVE <> 0 ORDER BY 1 COLLATE NOCASE")]
json.dump({"categories": cats, "payees": payees},
          open("data/extraction_eval_catalog.json", "w"), indent=2, ensure_ascii=False)
print(len(cats), "categories,", len(payees), "payees")
PY
```

### 2. Prepare representative statement files

Place representative `.pdf`, `.txt`, or image statements under the local, gitignored
`data/samples/` directory. Keep original statements and any goldens derived from them out of
version control — they contain your financial data.

Don't anonymize a PDF by re-exporting it from a document editor: that changes glyph runs,
font metrics, and column boundaries, so it may no longer reproduce extraction problems in the
bank's original format. The production extractor passes PDFs directly to Poppler's
`pdftotext`, which handles the common owner-password-with-empty-open-password case; PDFs that
require an open password are rejected.

#### Public sample statements

Some banks and educators publish sample statements with fictional account holders. They
cover layouts, languages and number formats that your own statements may not, and you can
share results on them. They are not ours to redistribute, so download them yourself into a
separate gitignored folder, for example `data/samples-public/`, and score them with
`--statements data/samples-public --prompt-dir ""` so the result reflects the built-in prompt.

| File | Source | What it exercises |
|------|--------|-------------------|
| Impact Bank checking (scan, 2 pages) | <https://www.impact-bank.com/user/file/dummy_statement.pdf> | image input; dates with no year anywhere; check numbers; page 2 lists the page-1 rows again by type |
| RBC chequing, French (2004) | <https://www.rbcroyalbank.com/fr/services-bancaires/_assets-custom/pdf/eStatementPersChequing_F.pdf> | French, decimal comma, several rows under one date |
| C1st credit card worksheet | <https://www.c1stcreditunion.com/webres/File/It's%20a%20Money%20Thing/Activity_B_02_Credit_Card_Statement.pdf> | `DD MM` row dates beside an `MM/DD/YY` closing date; a page of quiz questions that must yield nothing |
| Federal Reserve card sample (in a UGA handout, page 2) | <https://www.fcs.uga.edu/docs/credit_ho_5.pdf> | image; payment with a trailing minus; fee and interest sections; callout circles over the text |
| Musterbank Girokonto, German | <https://uni-koeln.de/sites/international/aaa/92/92pdf/REFUGEES_pdf/92pdf_ARS_Kontoauszug_Beispiel_2021_07.pdf> | German; amounts with a trailing `+`/`-`; descriptions over several lines |
| Sparkasse Girokonto demo, German | <https://www.ksk-tut.de/content/dam/myif/ksk-tuttlingen/work/dokumente/pdf/allgemein/Demokontoauszug.pdf> | German; signed amounts in Soll/Haben columns; a text layer with broken umlauts and stray glyphs |
| "Your Bank" current account, UK | <https://the-bac.edu/assets/documents/departments/admissions%20financial%20aid/admitted%20students/international/bank%20statement%20example%20final.pdf> | Money out / Money in columns; malformed totals such as `£44.079.83` |

The Impact Bank PDF has no text layer, so MMXC rejects it as a PDF; render its pages to images
first (`pdftoppm -r 150 -png dummy_statement.pdf impact-checking-page`) and grade them as image
statements. For the UGA handout, render only page 2 (`-f 2 -l 2`).

A statement that prints no year is dated relative to `--current-date`, so pin that flag to the
date its golden was written for; otherwise the expected year changes once a year passes.

### 3. Generate goldens, then correct them by hand

```sh
just extraction-eval-generate
# or: go run ./cmd/extraction-eval --generate --statements data/samples --catalog data/extraction_eval_catalog.json
```

For each statement under `--statements` (default `data/samples`), this runs **one** extraction
and writes `<statement>.expected.json` next to it. These are first drafts — open each one and
**fix every wrong field** (missed rows, wrong amounts/dates/types, bad categories) until it is
known-good. The corrected file is the golden the model is graded against.

### 4. Score models against the corrected goldens

```sh
just extraction-eval
# or: MMXC_EXTRACTION_EVAL_MODELS="qwen3:30b,llama3.1:8b" go run ./cmd/extraction-eval \
#       --statements data/samples --catalog data/extraction_eval_catalog.json
```

Each statement is re-extracted per model and compared to its golden. Statements with no
golden yet are skipped with a reminder to generate-then-correct.

Env vars used by the recipes: `MMXC_LLM_BASE_URL`, `MMXC_LLM_API_KEY`, `MMXC_EXTRACTION_EVAL_MODELS`,
`MMXC_EXTRACTION_EVAL_STATEMENTS`, `MMXC_EXTRACTION_EVAL_CATALOG`.

Key flags (`go run ./cmd/extraction-eval --help` for all):

| Flag | Default | Purpose |
|------|---------|---------|
| `--statements` | `data/samples` | a directory of statements, or a comma-separated list of files |
| `--catalog` | `data/extraction_eval_catalog.json` | categories (and payees) for the prompt |
| `--golden` | alongside each statement | directory for `<name>.expected.json` goldens |
| `--models` | `qwen3:30b` | models to grade; every statement goes to each, so image statements need multimodal models |
| `--generate` | off | (re)write goldens from one extraction instead of scoring |
| `--amount-epsilon` | `0.01` | amount tolerance when matching rows |
| `--prompt-dir` | `data/prompts` | where a `statement.txt` override is read from; `""` grades the built-in |
| `--current-date` | today | date injected into the prompt; it settles the year of a statement that prints none |
| `--temperature` / `--reasoning-effort` | `0` / `none` | sampling options |
| `--timeout` | `600s` | per-call timeout |
| `--api-key` | `$MMXC_LLM_API_KEY` | bearer token for the endpoint |
| `--verbose` | off | per-statement detail plus the complete raw model response on failures; repeat to print the rendered extraction prompt and raw responses for every extraction |

For PDF inputs, the double-verbose rendered prompt contains the exact text
extracted by MMXC's Poppler `pdftotext -layout` invocation. Inspect the statement
text between the prompt markers to distinguish a PDF-reading failure from a
model-generation failure.
Image payloads are reported by count but their base64 data is not printed.

Supported statement types: `.pdf` and `.txt` (sent as text), and
`.png/.jpg/.jpeg/.webp` (sent as an image, so the model must be multimodal). PDFs
are reduced to text with the same `pdftotext` invocation the app uses.

### Golden format (`ExtractedStatement`)

```json
{
  "account_name": "Everyday Chequing Account",
  "account_number": "****4821",
  "period_start": "2025-03-01",
  "period_end": "2025-03-31",
  "closing_date": "2025-03-31",
  "closing_balance": 2354.10,
  "has_closing": true,
  "transactions": [
    { "date": "2025-03-03", "type": "Withdrawal", "amount": 84.20,
      "description": "POS PURCHASE GREENLEAF GROCERS",
      "payee": "Greenleaf Grocers", "category": "Food: Groceries" }
  ]
}
```

Single-account goldens retain the flat format above for compatibility. Multi-account statements use
an `accounts` array whose entries contain the same header fields and `transactions` array. Empty
transaction arrays are significant: they grade whether the model preserved an account section that
only reports an opening/closing balance. `type` is `Withdrawal` or `Deposit`; `amount` is always
positive. Committed single- and multi-account synthetic examples live at
`cmd/extraction-eval/testdata/`.

### Scoring & output

Transactions are matched greedily, preferring amount + type + date, then amount + type, then
amount alone (within `--amount-epsilon`). For matched pairs the eval reports per-field
accuracy. Metrics per model:

- **recall** — golden rows that were found (did it miss any?)
- **precision** — got rows that matched a golden row (did it invent any?)
- **date / type / desc / payee / category** — field accuracy over matched pairs. `desc`
  tolerates the model cleaning the text (containment counts); blank golden payee/category are
  not graded.
- header notes (period, closing balance, account number) in `--verbose`
- mean latency

The process exits non-zero only on **operational** failures (an extraction call errored or
the response could not be parsed) — model inaccuracy is reported, not treated as a build
failure, since the whole point is to compare imperfect models.
