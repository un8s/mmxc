package mmxc

import "embed"

// UI assets live as real files under assets/ (with editor syntax highlighting)
// and are compiled into the binary via go:embed. Edit the asset files, not
// these declarations.

//go:embed assets/templates.html
var templates string

//go:embed assets/app.css
var css string

//go:embed assets/themes/*.css
var themeAssets embed.FS

//go:embed assets/app.js
var js string

// promptAssets holds the built-in guidance of the overridable LLM prompts; see
// prompts.go.
//
//go:embed assets/prompts/*.txt
var promptAssets embed.FS

//go:embed assets/favicon.svg
var favicon string

// schemaSQL is the complete canonical MMEX *_V1 DDL. It is the single source of
// truth for both creating a brand-new database (see newdb.go) and building the
// synthetic test fixture (fixture_test.go), so the two can never drift.
//
//go:embed assets/schema.sql
var schemaSQL string

// auditSchemaSQL creates the audit log's own table. It is applied to the
// separate audit database (see audit.go), never to the MMEX file.
//
//go:embed assets/audit_schema.sql
var auditSchemaSQL string

// seedCategoriesSQL is the default category tree stamped into a new database
// (see newdb.go). Currencies are seeded from the Go list in newdb.go because the
// setup UI needs the same list to offer the base-currency choice.
//
//go:embed assets/seed_categories.sql
var seedCategoriesSQL string

// version (YY.M.D) and history (tab-separated "date<TAB>subject" per line) are
// snapshotted from git at build time by `just gen`; the app has no git access at
// runtime. See parseHistory.
//
//go:embed assets/version.txt
var versionRaw string

//go:embed assets/history.txt
var historyRaw string
