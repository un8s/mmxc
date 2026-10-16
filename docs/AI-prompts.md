# Custom LLM prompts

Small local models respond to wording differently, and every bank lays out its
statements in its own way. You can therefore replace the instructions MMXC gives
the model for **Ask** and for **statement import** with your own text files,
without rebuilding.

## What you can change

Each of these prompts has two parts:

- **The contract** stays in the code: the JSON shape the model must return, the
  tool list and its argument schemas, the allowed values (`type`, `status`,
  `sort`, …) and the data MMXC inserts (date, account names, categories,
  statement text). MMXC's parser depends on it, so editing it could only break
  things.
- **The guidance** is the part you can replace: which tool to use for which
  kind of question, how to read "last month", when to ask for clarification,
  which statement rows are not transactions, which date column to use, and so
  on.

| File | Replaces the guidance of |
|------|--------------------------|
| `assistant.txt` | the Ask tool-selection prompt |
| `statement.txt` | statement extraction (Review → import) |

## How to override

1. In **Settings → LLM**, click *Download built-in* next to the prompt. The
   built-in files are also in the source tree under
   `internal/mmxc/assets/prompts/`.
2. Edit the file and save it under the same name in the **Prompt Directory**
   (`prompt_dir`, default `data/prompts`).
3. Your next Ask question or statement upload uses it. A restart is not needed.

Settings shows which prompts are custom. Delete the file to go back to the
built-in text.

An override replaces the built-in guidance completely, so start from the
downloaded file and change what you need. Lines starting with `#` at the top of
the file are comments and are not sent to the model.

## The version header

Every built-in file starts with `# mmxc-prompt-version: N`. Keep that line in
your copy. When a new MMXC version changes the built-in guidance in a way that
matters (a new tool or field, for example), N goes up and Settings warns that
your file was written for an older version. Compare your file with the new
built-in, merge what you need, and update the header to clear the warning. A
file without the header works, but it also gets a warning, because MMXC cannot
tell how old it is.

An empty override (comments only) is ignored, and MMXC uses the built-in
guidance with a warning.

## Measure before you rely on it

Both evals read the same directory through `--prompt-dir` (default
`data/prompts`, like the app) and print whether they are grading the built-in
or your override. Run them against your model before and after a change; see
[AI-eval.md](AI-eval.md).

Prompt files often hold bank-specific wording. Keep them under `data/`, which
is not committed.
