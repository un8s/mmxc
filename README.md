# MMXC

[![test](https://github.com/un8s/mmxc/actions/workflows/test.yml/badge.svg)](https://github.com/un8s/mmxc/actions/workflows/test.yml)

A small, self-hosted personal-finance web app that reads and writes a
[Money Manager Ex (MMEX)](https://github.com/moneymanagerex/moneymanagerex)
`.mmb` SQLite database.

MMXC is **not** a reimplementation of MMEX. It is a focused browser interface
over the *same* database file, built around a monthly review and LLM-assisted
statement-import workflow. Data entered in MMEX shows up in MMXC, and data
entered in MMXC stays valid in MMEX.

The interface is English by default and can be translated into other languages
with your configured LLM (see [Translation](#translation)). Your data is
language-independent: a new database can be based on any of the bundled world
currencies, and MMXC reads whatever accounts, categories, and currencies already
exist in your `.mmb`.

> ⚠️ **MMXC writes to your real financial database.** Writes are only possible
> when the database file is writable, are gated behind a write-lock probe, and
> take a full file backup before every change — but you should still keep your
> own backups and never run MMXC and the MMEX desktop app against the same file
> at the same time. MMXC is young software: try it on a copy first.
>
> **LLM features send data to your configured endpoint.** Statement import and
> the assistant send statement contents or transaction data to whatever
> `llm_base_url` points at. With a local Ollama box that never leaves your
> network; with a hosted provider it does. The LLM features are optional.

## Principles

- **Compatibility first** — no new tables or columns; MMEX keeps working on the
  same file.
- **The model proposes, you decide** — every AI suggestion is reviewed before it
  is written, and the assistant is read-only.
- **Local first** — your data and statements stay on your machine; a local model
  is a first-class setup.
- **Small surface** — the monthly review and statement import come before broad
  feature parity.

The reasoning and trade-offs behind these are in [docs/DESIGN.md](docs/DESIGN.md).

## Features

- **Accounts, Categories, Payees** — browse and edit, each with a filterable
  transaction list (date range, type, amount, status, search, …).
- **Transactions** — manual entry and editing of withdrawals, deposits, and
  transfers; deletion is a soft `Void` (MMEX-compatible). Permanent deletion is
  available only behind the opt-in `allow_transaction_delete` setting.
- **Budgets** — view existing MMEX budgets; create a new budget year from empty
  or by copying an existing one; edit category amounts.
- **Monthly review + statement import** — upload PDF or image statements, extract
  candidate transactions with an LLM, or upload an OFX/QFX ("Microsoft Money" /
  "Quicken") or QIF transaction download, which is read without the LLM; review
  and edit the candidates (with duplicate and
  recurring warnings), then import. Nothing is written without your approval.
  Statements are archived locally and attached to their accounts, one statement
  can cover several accounts, and MMXC learns payee/category mappings from your
  corrections (viewable and editable under *Learned Mappings*).
- **Assistant** — ask questions about your finances in plain language ("How much
  did I spend on groceries last month?"). The model only proposes a constrained,
  read-only query; MMXC runs it and shows the totals, filters, and matching
  transactions behind every answer. It never runs arbitrary SQL and never writes.
- **Bring your own model** — any OpenAI-compatible endpoint works: a local
  [Ollama](https://ollama.com) box (the privacy-preserving default),
  [OpenRouter](https://openrouter.ai), OpenAI, vLLM, or LM Studio. Set
  `llm_base_url` (plus `llm_api_key` for hosted providers) in Settings.
- **Reports** — overview (income vs. expenses), spending trends, expense
  categories and payees, anomalies, budget overruns, and recurring-transaction
  review.
- **Encryption at rest (optional)** — convert a database to MMXC's own
  encrypted `.mmbx` format (and back) from Settings. See
  [Encrypted databases](#encrypted-databases) for the trade-off.
- **Audit log** — a record of the changes MMXC makes, kept in its own database
  (off, actions-only, or detailed) rather than in the process log.
- **Optional sign-in** — OIDC login (e.g. Pocket ID, Authentik, Keycloak) for
  deployments exposed beyond localhost.
- **Themes** — light/dark/system modes and CSS themes.
- **Compatibility-first** — works within the existing MMEX `*_V1` schema, refuses
  databases older than the supported version, and shows compatibility status in
  Settings.

## Requirements

- Go 1.26+ (to build) — or use the Docker image
- A MMEX `.mmb` database — bring an existing one, or let MMXC create a new one
  (see below); no Money Manager Ex install required. MMEX's own encrypted
  `.emb` format is not supported.
- Poppler's `pdftotext` (required for PDF statement import; included in the
  Docker image)
- An OpenAI-compatible LLM endpoint (optional, only for statement import and the
  assistant) — e.g. [Ollama](https://ollama.com) running locally. Ollama's default
  4096-token context window is too small for a full statement, so start it with
  `OLLAMA_CONTEXT_LENGTH=32768 ollama serve`. Without an endpoint, everything
  else works and statements can still be attached manually.
- [`just`](https://github.com/casey/just) (optional, for the dev recipes)

## Quick start

```sh
# 1. Configure
cp mmxc.example.yaml mmxc.yaml
#    edit mmxc.yaml: set db_path to the folder containing your .mmb files

# 2. Run
just run            # or: go run ./cmd/mmxc -config mmxc.yaml
```

Then open <http://localhost:8080>. Build a standalone binary with `just build`
(output in `bin/mmxc`), and run the tests with `just check`. Without `just`:
`go build -o bin/mmxc ./cmd/mmxc` and `go test ./...`.

Point the LLM settings at your endpoint under **Settings** (or `llm_base_url`
and `llm_model` in `mmxc.yaml`); Settings can list the models the endpoint
offers. One model handles both Ask and statement import; to import statements
from images or screenshots, choose a multimodal model that accepts images.

### Starting from a new database

You don't need Money Manager Ex to create an empty database. Either:

- **CLI:** `mmxc init --db data/finances.mmb --currency USD` stamps out a fresh
  `.mmb` (see `mmxc init --list-currencies` for the choices), then start MMXC
  normally.
- **Web:** just start MMXC pointed at a `db_path` folder with no `.mmb` files —
  it serves a first-run setup page where you pick a base currency, creates
  `mmxc.mmb` in that folder, then continues into the app once the database is
  created.

New databases come seeded with the common currencies and a default category
tree, so you can start adding accounts and transactions right away when the
`.mmb` file is writable. Make the file read-only to run MMXC read-only. Once a
database exists, **Settings → New Database** adds more.

## Configuration

Settings are resolved as **built-in defaults → YAML file → environment
overrides**. `db_path` is scanned once at startup for `.mmb` and encrypted
`.mmbx` files; Settings shows those files in a database dropdown and remembers
your choice (`selected_db`). Write mode is derived from the selected file's
filesystem permissions. See `mmxc.example.yaml` for the full annotated list.
The live `mmxc.yaml` is gitignored, since it may hold an API key.

A few settings can also be set initially via environment variables, for
container deployments:

| Variable | Purpose |
| --- | --- |
| `MMXC_CONFIG` | Path to the YAML config file (locator only) |
| `MMXC_DB_PATH` | Folder scanned at startup for `.mmb` / `.mmbx` databases (default `data`). Naming one file instead pins it: every restart opens that file, whatever Settings last selected |
| `MMXC_DB_PASSWORD` | Password for an encrypted `.mmbx` database; without it the unlock page asks at startup |
| `MMXC_DB_PASSWORD_FILE` | Path to a secret file holding that password (Docker/systemd secrets, kept on tmpfs). Wins over `MMXC_DB_PASSWORD` |
| `MMXC_HTTP_ADDR` | Listen address (e.g. `:8080`) |
| `MMXC_AUTH_ENABLED`, `MMXC_OIDC_*`, `MMXC_SESSION_*` | Optional OIDC login gate |

### Encrypted databases

An encrypted database is a `.mmbx` file, written through an Adiantum VFS; plain
`.mmb` files are never touched. **MMEX cannot open a `.mmbx`** — this is MMXC's
own format, not MMEX's `.emb` — so converting back to a plain `.mmb` copy is the
way to use MMEX again. There is no password recovery. The password is never
stored in the YAML file: MMXC asks on an unlock page at startup, or reads it from
`MMXC_DB_PASSWORD_FILE` / `MMXC_DB_PASSWORD` for unattended restarts.

What the encryption does and does not do:

- **It keeps the contents secret.** Without the password, a stolen disk, a
  copied `.mmbx` or one of its backups reveals nothing of your data. This holds
  even against someone who can also change the file.
- **It does not detect changes.** Someone who can write to the file still cannot
  read it or put chosen values into it: changing any byte turns that whole 4 KiB
  block into garbage. But they can damage the file, or put back blocks from an
  older copy, and MMXC will not notice. Someone holding several copies over time
  (successive backups, say) can tell which blocks changed between them, though
  not what changed. Keep your own backups somewhere you trust.
- **It protects the file, not a running MMXC.** Once unlocked, the data is
  available to whoever can use the app, so put sign-in in front of it (see
  [Deployment](#deployment)).
- **The password is the key.** Anyone with a copy can try passwords offline for
  as long as they like, so use a long passphrase.

Statements, and backups of a plain database, are not encrypted.

### Translation

Set `translation_language` (e.g. `de`, `fr`) to have your LLM translate the UI.
Translations are cached per language under `translation_dir`, so only the first
page load in a new language is slow. See [docs/i18n.md](docs/i18n.md).

## Deployment

MMXC ships as a single static binary in a small Alpine image (pure-Go SQLite, no
C toolchain; `amd64` and `arm64`). There is no published image: build it from
this repository with `docker compose up -d --build` (or `docker build -t mmxc .`),
so every rebuild also picks up current base-image security fixes. See
**[docs/DOCKER.md](docs/DOCKER.md)** for the Docker / Podman setup, including the
`docker-compose.yml` in the repository root.

MMXC is a single-user app. If it is reachable beyond your own machine, enable
the OIDC sign-in and/or put it behind a reverse proxy (see `mmxc.example.yaml`
and docs/DOCKER.md).

## How it's built

One Go binary on the standard library, with two direct dependencies (a pure-Go
SQLite driver and a YAML parser), server-rendered HTML and a single plain
JavaScript file: no web framework, no cgo, no frontend build step. Writes go
through a lock probe and a full backup; AI output is validated by code before
anything is shown or stored; and model quality is measured with evals that
reuse the app's own prompts and parser. The decisions and their costs are in
**[docs/DESIGN.md](docs/DESIGN.md)**.

## Documentation

- **[docs/DESIGN.md](docs/DESIGN.md)** — design decisions and trade-offs
- **[docs/DOCKER.md](docs/DOCKER.md)** — container deployment
- **[docs/AI-assistant.md](docs/AI-assistant.md)** — design of the read-only
  assistant
- **[docs/AI-prompts.md](docs/AI-prompts.md)** — tuning the Ask and statement
  prompts for your model or bank
- **[docs/AI-eval.md](docs/AI-eval.md)** — evals for comparing models on assistant
  tool-calling and statement extraction
- **[docs/i18n.md](docs/i18n.md)** — how UI translation works
- **[mmxc.example.yaml](mmxc.example.yaml)** — every setting, annotated

## License

[MIT](LICENSE)
