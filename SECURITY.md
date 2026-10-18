# Security policy

## Reporting a vulnerability

Please report security problems **privately** through GitHub's
[private vulnerability reporting](https://github.com/un8s/mmxc/security/advisories/new)
(the *Report a vulnerability* button on the Security tab), not in a public
issue. Include what you found, how to reproduce it, and what an attacker gains.

MMXC is maintained in spare time. Reports are read and answered on a
best-effort basis; a confirmed issue is fixed on `main` and credited in the
advisory unless you prefer otherwise.

## Supported versions

Only the latest commit on `main`. There are no release branches; build from
current source (see [Deployment](README.md#deployment)).

## Scope

MMXC is a single-user, self-hosted app. In scope, for example:

- getting past the OIDC sign-in or forging a session when sign-in is enabled
- writing to the database when it is open read-only, or without the write-lock
  probe and the backup that precede every change
- reading files outside the configured database, statement, backup and audit
  folders, or writing anywhere else
- cross-site scripting or request forgery against the web UI
- a crafted statement file, bank download or `.mmb` that makes MMXC change data
  without the user confirming it, or run code
- recovering the contents of an encrypted `.mmbx` without the password

## Known limitations

These are documented design trade-offs, not vulnerabilities:

- **Encryption gives confidentiality, not integrity.** A `.mmbx` keeps its
  contents secret, but damaged or reverted blocks go undetected, and successive
  copies show which blocks changed. Statements, and backups of a plain database,
  are not encrypted. See [Encrypted databases](README.md#encrypted-databases).
- **Without sign-in there is no access control.** With `auth_enabled` off,
  anyone who can reach the port can use the app. Enable OIDC or put it behind
  an authenticating proxy before exposing it beyond your own machine.
- **The LLM endpoint sees your data.** Statement import and Ask send statement
  contents and transaction data to the configured `llm_base_url`, by design.
  Whoever runs that endpoint is trusted with it.
- **Model output can be wrong.** Extracted transactions and Ask answers can be
  inaccurate, including when a statement is written to mislead the model. Code
  validates the output and nothing is imported until you approve it.
