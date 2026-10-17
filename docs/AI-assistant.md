# The Ask assistant

**Ask** answers natural-language questions about your finances ("How much did I
spend on groceries last month?") using your configured LLM. It is read-only: the
model never runs SQL and never changes data. It chooses from a small set of
app-owned tools; MMXC executes them and shows the answer together with the
totals and the matching transactions behind it.

It needs a configured LLM endpoint and text model (Settings → LLM). Without them
the nav entry is disabled and `/assistant` returns an explanatory 503.

## How it works

`assistant.go` runs a short tool loop (at most 4 steps) per question:

1. MMXC builds a prompt (`BuildAssistantToolPrompt`) containing the current date,
   the account names, the tool definitions and rules, and the transcript of
   earlier tool calls in this question. The rules on choosing tools can be
   replaced with your own file; see [AI-prompts.md](AI-prompts.md).
2. The model must reply with one JSON object: either `{"action":"tool",…}` or
   `{"action":"final","answer":…}`. A malformed reply or a failing tool call gets
   one automatic repair round (`repairAssistantToolDecision`).
3. A tool call is validated and executed by Go; its result is appended to the
   transcript and the loop repeats until the model returns `final`.

The model never receives the database, only the question, account names, and the
tool results it asked for.

### Tools

| Tool | Purpose |
| --- | --- |
| `find_account`, `find_category`, `find_payee` | Look up names (substring match). The model must resolve names this way before filtering; filters accept **exact** names only. |
| `spending_summary` | Count and withdrawal/deposit/net totals for a filter ("how much", "how often"). With `group_by: "month"` it also returns one row per calendar month of the range, empty months included ("each month this year"); the answer shows them as a table. At most 36 months. |
| `compare_summary` | The same totals for two explicit periods, plus the delta. |
| `read_transactions` | Individual rows ("show", "biggest", "latest"). |
| `budget_status` | Budget versus actual for one budget year ("how much budget is left for groceries this year?", "how much over budget was I last year on hobbies?"), for one category and its subcategories or for every budgeted category. |
| `ask_clarification` | Ask the user to pick among options. |

Filters: `from`/`to` (YYYY-MM-DD), `account`, `payee`, `category`,
`uncategorized`, `description_contains`, `type`, `status`, `amount_min`/`max`,
and for `read_transactions` `sort` and `limit`. `budget_status` takes only
`year` (`YYYY`, default the current year) and an exact `category`. Arguments are decoded strictly
(unknown fields are rejected) and `type`, `status`, `sort` and dates are
validated against closed sets, so a bad or hostile model reply fails validation
instead of reaching the database.

### Clarification

When a lookup returns several plausible matches the prompt requires the model to
call `ask_clarification` rather than pick one. The page shows the options as
buttons; choosing one posts the transcript back with the answer appended
(`clarification_answer`) and the loop continues. The transcript travels as a
hidden form field, so nothing is stored server-side.

## What you see

- The model's **answer** — brief by instruction; with more than 5 transactions it
  should summarize and point at the table.
- **Totals** (count, withdrawals, deposits, net), computed in Go from the
  returned rows. For a budget answer the chips are budget, actual and
  remaining instead.
- The **transaction table**, using the normal editable transaction list: with a
  writable database you can fix a category, payee or status right from the
  answer.
- For a budget answer, a **budget table** (budget, actual, remaining and percent
  used per budget line, overruns first) with a link to that year on the Budgets
  page, in place of the transaction table.
- **Warnings**, e.g. which exact account/payee/category was carried forward from
  a lookup, or that split totals count only the matching lines.
- **History** of past questions, kept in your browser's `localStorage`
  (per-browser, never sent to the server).
- Optional **Query details** (tool transcript), shown only when
  `assistant_query_details: true`.
- A flag button that appends the question to `data/assistant-feedback.txt` so
  bad answers can later become eval cases. The file is plain text and holds the
  question as typed — delete it if the questions are sensitive.

## Design rules (keep these when changing it)

- **The model proposes, Go executes.** No SQL, table names, or free-form
  expressions ever come from the model. Don't "simplify" this by passing a query
  string through; closed, validated arguments are what contain prompt injection.
- **Don't trust the answer text; the table is the source of truth.** The final
  answer is written by the model from the tool results, so it can be wrong or
  misquote a number. The totals and the table beneath it are computed by Go, and
  the prompt tells the model to ground the answer only in tool results. Anything
  that must be exact belongs in the totals, not the prose.
- **Category totals follow the split, not the whole transaction.** For a category
  filter the amount attributed to it is the matched `SPLITTRANSACTIONS_V1` lines
  plus non-split rows with that category (`assistantCategoryTotals`), not the
  full `TRANSAMOUNT`. `summarizeTransactions` over-counts splits and must not be
  used for category or uncategorized questions. Category filters include child
  categories, which the warning states. Uncategorized works the same way.
- **One query path.** Tools reuse `transactionFilter` / `listTransactions`, so the
  assistant and the account/category/payee lists can't drift. `budget_status`
  likewise reuses `listBudgetEntries`, so its figures are the Budgets page's:
  a budget line counts transactions on its own category only (a parent budget
  does not include its subcategories' spending), over the whole calendar year.
  A category argument selects that category's budget lines and those of its
  subcategories.
- **Read-only.** The assistant uses the normal connection and only reads. Edits go
  through the existing transaction endpoints with their write-mode, lock and
  backup checks.
- **Replay without the model.** After an answer, the page URL is
  `/assistant?q=…&tool=…&args=…`; it re-executes that one tool call, not the
  model. That is why editing a transaction from the results returns to the same
  rows. Lookup and clarification calls are not replayable.

## Limits and known rough edges

- Prompt ≤ 1000 characters, clarification answer ≤ 500.
- `read_transactions`: default 20 rows, max 50.
- `spending_summary`/`compare_summary`: default 500 rows, max 1000. Totals are
  computed over the fetched rows, so a filter matching more than that is
  **silently undercounted**; narrow the period.
- Summaries default to `Withdrawal` unless the question implies otherwise;
  transfers are excluded from spending questions that way.
- For `compare_summary` the table shows both periods' rows together while the
  totals chips show period A only; the comparison itself is in the answer.
- Budgets are read-only from Ask, and only yearly budgets (`YYYY` names) are
  understood; MMEX month budgets are not. `budget_status` returns at most 40
  budget lines when no category narrows it.
- Quality depends heavily on the model. Small local models can need more
  clarifications or mis-pick tools.

## Changing the prompt or tools

The prompt is a long list of behavioural rules, and small edits can regress
unrelated questions. The rules live in `internal/mmxc/assets/prompts/assistant.txt`;
the schema, tool list and allowed values stay in `BuildAssistantToolPrompt` because
the parser depends on them. After any change to either, the tool set, or the
parser (bump the `mmxc-prompt-version` header when a custom copy of the rules
would need the change too):

1. Add or update cases in `docs/assistant_eval_cases.json`.
2. Run `go test ./...` and then `just assistant-eval` against the model you
   deploy with.

The eval calls the app's real prompt builder and parser, so there is no copy to
keep in sync. Cases must be synthetic: invented accounts, payees and
transactions only, never anything from a real database. See
[AI-eval.md](AI-eval.md) for flags and the file format.

## Privacy

Questions, account names, and tool results (which include payees, amounts and
dates for the rows the model asked about) go to the endpoint in `llm_base_url`.
That stays on your network with a local Ollama; with a hosted provider it does
not. Prompts and responses are only logged when `log_llm` is enabled.
