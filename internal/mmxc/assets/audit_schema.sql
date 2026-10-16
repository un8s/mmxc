-- Schema for MMXC's audit log.
--
-- This is deliberately NOT part of the MMEX database: the MMEX *_V1 schema is
-- fixed and must stay readable by MMEX, and the audit log has to keep working
-- while the MMEX file is open read-only. It lives in its own SQLite file (see
-- auditStorePath), which is opened through the same openSQLite as everything
-- else -- so when the MMEX database is encrypted, this file is encrypted with
-- the same key and the same Adiantum VFS.

CREATE TABLE IF NOT EXISTS AUDITLOG (
    ID     INTEGER PRIMARY KEY AUTOINCREMENT,
    TS     TEXT NOT NULL,              -- RFC3339, always UTC
    ACTOR  TEXT NOT NULL DEFAULT '',   -- OIDC identity, empty when the login gate is off
    ACTION TEXT NOT NULL,              -- what happened: verb plus row ids only
    DETAIL TEXT NOT NULL DEFAULT ''    -- the sensitive part; written only at the "detailed" level
);

CREATE INDEX IF NOT EXISTS IDX_AUDITLOG_TS ON AUDITLOG (TS);
