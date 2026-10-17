# Design notes

The decisions that shape MMXC, why they were made, and what they cost. For how
to run it, see the [README](../README.md).

## Principles

- **Compatibility first.** MMXC works on the Money Manager Ex file itself, not on
  a copy or an export. Anything MMEX wrote must show up here, and anything MMXC
  writes must stay valid in MMEX.
- **The model proposes, you decide.** Language models suggest transactions,
  payees, categories and queries; plain code validates and runs them, and
  nothing reaches the database without your approval.
- **Local first.** Data, statements and the audit log stay on your machine or
  server. A local model is a first-class setup, not a fallback.
- **Small surface.** The monthly review, statement import and everyday
  transaction work come first. Broad feature parity with MMEX is not a goal.

## Data and compatibility

**No new tables, no new columns.** MMXC reads and writes only the MMEX `*_V1`
schema. Its own state lives outside the database (the statement archive, the
audit log, the translation cache) or, where it must travel with the data, in
the key-value table MMEX already has: learned import rules are a JSON value in
`INFOTABLE_V1`. *Cost:* some things are awkward to store, but no MMEX upgrade or
backup can ever trip over a table it doesn't know.

**Deletion is a void.** Deleting a transaction sets `STATUS = 'Void'`, which
MMEX understands; every balance and report excludes voided rows. A physical
delete exists only behind an opt-in setting, and refuses transactions MMEX ties
to attachments or investment holdings.

**IDs that don't collide with MMEX.** New rows take a microsecond timestamp as
their ID (or `max + 1` if that is already taken), so the two apps writing the
same file at different times don't race for the next small integer.

**Balances are computed, not stored.** An account's balance is its opening
balance plus its transactions, derived in SQL on every read. There is no cached
total to drift out of sync with what MMEX wrote.

**A compatibility gate, not a migration.** At startup MMXC checks the database's
`DATAVERSION` and that every table and column it uses exists. An older file is
refused with a pointer to upgrading it in MMEX. MMXC never migrates a schema it
doesn't own.

## Writing safely

Every change to the database goes through the same four steps:

1. Write mode is on only if the database file itself is writable. There is no
   setting to forget; `chmod a-w` makes MMXC read-only.
2. A `BEGIN IMMEDIATE` / `ROLLBACK` probe confirms no other program (a running
   MMEX, say) holds the file.
3. The whole file is copied to the backup folder.
4. The change runs in a transaction.

In read-only mode the database is opened with `mode=ro` and `query_only`, on
every pooled connection, so a bug cannot write by accident.

## AI features

**One transport, no provider abstraction.** Ollama, OpenRouter, OpenAI, vLLM and
LM Studio all serve the OpenAI chat-completions API, so MMXC speaks exactly that
and nothing else. One client, one code path, and any compatible server works.

**Contract in code, guidance in files.** Each prompt has two parts. The
*contract* (the JSON shape, the tool list, allowed values, the injected data)
lives in Go beside the parser that depends on it. The *guidance* (which tool
fits which question, which statement rows to skip) is a text file users can
replace without rebuilding. Tuning for a small model or an odd bank layout can
therefore never break parsing. See [AI-prompts.md](AI-prompts.md).

**Deterministic first, model last.** Imported rows are resolved by code first:
learned rules from your earlier corrections, then exact and fuzzy name matching
against your payees and categories, then the payee's own history (its usual
category, or the split of an identical recurring charge), then duplicate
detection. Only names that are still unresolved go to the model, in one batched
call that must pick from your existing payees and categories rather than invent
new ones; its picks are marked for you to confirm. Reading the file is a
separate step: PDFs and images need the model to extract rows, while OFX, QFX
and QIF downloads are parsed by code, so they import without any model. When a
model is configured, their leftover names still get the same batched
suggestion pass.

**A read-only assistant with a narrow interface.** Ask never runs SQL. The model
chooses from a few app-owned tools with validated arguments; Go executes them
and shows the totals and transactions behind every answer. See
[AI-assistant.md](AI-assistant.md).

**Translation as a cache, steered from the template.** There are no message
catalogs to maintain. Each UI string is written once, in English, as
`{{.T "Balance" "running account balance after this transaction"}}`: the
English text is the key, and the optional second argument tells the model
where the string appears. Short
labels are where machine translation usually goes wrong (*Balance*, *Split*,
*Void*, *Status* each mean different things in different places), so the
context travels with the string, and the same word used in two senses gets two
cache entries. About 200 of the 466 distinct template strings carry guidance.
A string is translated by a small model the first time it is shown and kept in
one plain JSON file per language, which you can correct by hand; a whole
language is a few hundred one-time calls, then none. Output is checked before
it is kept: a translation whose `%s` placeholders differ from the source, or
that is implausibly long (the model translated the prompt instead), is
rejected and English is shown. No model, or an error, also means English, so
the interface never waits on or breaks because of translation. *Cost:* the
first visit to a page in a new language is slow, one call per new string. See
[i18n.md](i18n.md).

**Measured, not guessed.** Two evals score models on assistant tool-calling and
on statement extraction. They call the app's own prompt builder, parser and
HTTP client, so there is no copy to drift, and their fixtures are entirely
invented data. See [AI-eval.md](AI-eval.md).

**Fail loudly on silent truncation.** The OpenAI API has no per-request context
size, and an over-long prompt is cut off without an error. MMXC compares its own
token estimate with the count the server reports and stops with an actionable
message when the server clearly dropped part of the prompt.

## Privacy and security

**Encryption at rest, honestly scoped.** An encrypted database (`.mmbx`) is
written through an Adiantum VFS: contents stay secret from anyone holding a
copy, but there is no integrity check, so damage or rollback goes unnoticed.
The [README](../README.md#encrypted-databases) spells out what this does and
does not protect. *Cost:* MMEX cannot open the encrypted format; Settings writes
a plain `.mmb` copy whenever you need one.

**The audit log is not the process log.** Process logs end up in journald or a
container log driver, which MMXC doesn't control and which are often readable
by more accounts than the database. The record of what MMXC changed therefore
lives in its own SQLite file, encrypted along with the database when that is.
The default level records only the action and row IDs; names, amounts and
account numbers are opt-in.

**Passwords never touch the config file.** The database password comes from an
unlock page at startup, an environment variable, or a secret file (the
Docker/systemd convention, kept on tmpfs).

## Stack

- **Go standard library** for HTTP and templates. No web framework, and two
  direct dependencies: the SQLite driver and a YAML parser.
- **Pure-Go SQLite** ([ncruces/go-sqlite3](https://github.com/ncruces/go-sqlite3),
  SQLite compiled to Wasm). No cgo, so builds are static and cross-compile for
  `amd64` and `arm64`; its writable VFS layer is what makes encryption possible.
- **Server-rendered HTML** with one plain JavaScript file for interactions.
  No build step and no frontend dependencies.
- **One package.** The application is a single Go package split into files by
  domain (accounts, transactions, review, reports, …). Moving code between files
  needs no import changes, and there are no interfaces that exist only for
  layering.
- **Tests on invented data.** Every database test builds a fresh one from the same
  schema file used to create real databases, seeded with hand-written data, so
  the fixture cannot drift from production and the repository holds no real
  financial data.
