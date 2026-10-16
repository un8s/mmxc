-- Purely synthetic seed data in the MMEX *_V1 schema, used only by the Go tests.
-- It contains no real financial data. IDs and structure are chosen to satisfy
-- the fixed-ID assertions in main_test.go (see fixture_test.go): payee Amazon = 14,
-- category Hobbies = 66 (under Leisure = 3), account 12, base currency 35, etc.

-- Currencies. 35 is the base currency (see BASECURRENCYID below); a second
-- currency exists only so that "base sorts first" ordering is observable.
INSERT INTO CURRENCYFORMATS_V1
  (CURRENCYID, CURRENCYNAME, PFX_SYMBOL, SFX_SYMBOL, DECIMAL_POINT, GROUP_SEPARATOR,
   UNIT_NAME, CENT_NAME, SCALE, BASECONVRATE, CURRENCY_SYMBOL, CURRENCY_TYPE)
VALUES
  (35, 'Test Dollar', '$', '',   '.', ',', 'Dollar', 'Cent', 100, 1.0, 'TSD', 'Fiat'),
  (2,  'Test Euro',   '',  ' E', '.', ' ', 'Euro',   'Cent', 100, 1.5, 'TEU', 'Fiat');

INSERT INTO INFOTABLE_V1 (INFOID, INFONAME, INFOVALUE) VALUES
  (1, 'MMEXVERSION',      '1.6.4'),
  (2, 'DATAVERSION',      '3'),
  (3, 'BASECURRENCYID',   '35'),
  (4, 'BASECURRENCYNAME', 'Test Dollar'),
  (5, 'DATEFORMAT',       '%Y-%m-%d');

-- Category tree. Top-level (PARENTID = -1): Leisure (3), Transfer (4), Food (20).
-- Transfers attach to the special top-level "Transfer" category.
INSERT INTO CATEGORY_V1 (CATEGID, CATEGNAME, ACTIVE, PARENTID) VALUES
  (3,   'Leisure',       1, -1),
  (4,   'Transfer',      1, -1),
  (20,  'Food',          1, -1),
  (66,  'Hobbies',       1, 3),
  (1,   'Dining',        1, 20),
  (16,  'Misc',          1, 20),
  (30,  'Groceries',     1, 20),
  (86,  'Toiletries',    1, 20),
  (102, 'Subscriptions', 1, 20);

INSERT INTO PAYEE_V1 (PAYEEID, PAYEENAME, CATEGID, NUMBER, WEBSITE, NOTES, ACTIVE, PATTERN) VALUES
  (1,  'Acme Utilities', NULL, '', '', '', 1, ''),
  (2,  'Grocery Mart',   30,   '', '', '', 1, ''),
  (3,  'Coffee Stop',    1,    '', '', '', 1, ''),
  (14, 'Amazon',         66,   '', '', '', 1, '');

-- Accounts: 12 (editable Checking, has month activity), 2 (Credit Card, transfer
-- target), 5 (Closed, to exercise the hide-closed-accounts path).
INSERT INTO ACCOUNTLIST_V1
  (ACCOUNTID, ACCOUNTNAME, ACCOUNTTYPE, ACCOUNTNUM, STATUS, NOTES, HELDAT, WEBSITE,
   CONTACTINFO, ACCESSINFO, INITIALBAL, FAVORITEACCT, CURRENCYID, STATEMENTLOCKED,
   STATEMENTDATE, MINIMUMBALANCE, CREDITLIMIT, INTERESTRATE, PAYMENTDUEDATE,
   MINIMUMPAYMENT, INITIALDATE)
VALUES
  (12, 'Everyday Chequing',   'Checking',    '', 'Open',   '', '', '', '', '', 1000.0, 'TRUE',  35, 0, NULL, 0, 0, 0, NULL, 0, NULL),
  (2,  'Rewards Credit Card', 'Credit Card', '', 'Open',   '', '', '', '', '', 0.0,    'FALSE', 35, 0, NULL, 0, 0, 0, NULL, 0, NULL),
  (5,  'Old Savings',         'Term',        '', 'Closed', '', '', '', '', '', 0.0,    'FALSE', 35, 0, NULL, 0, 0, 0, NULL, 0, NULL);

-- Transactions:
--   100  normal withdrawal — keeps payee 1 and category 66 "in use" (so their
--        deletes are rejected) and gives account 12 October-2025 activity.
--   186  split parent (CATEGID = -1), splits into 30 + 102.
--   187  second split parent referencing 102, so editing 186's splits doesn't
--        leave 102 unused (its delete must still be rejected).
INSERT INTO CHECKINGACCOUNT_V1
  (TRANSID, ACCOUNTID, TOACCOUNTID, PAYEEID, TRANSCODE, TRANSAMOUNT, STATUS,
   TRANSACTIONNUMBER, NOTES, CATEGID, TRANSDATE, FOLLOWUPID, TOTRANSAMOUNT,
   LASTUPDATEDTIME, DELETEDTIME, COLOR)
VALUES
  (100, 12, NULL, 1,  'Withdrawal', 50.0,  'Reconciled', '', 'seed utilities', 66, '2025-10-15T00:00:00', NULL, NULL, '2025-10-15T00:00:00', NULL, -1),
  (186, 12, NULL, 14, 'Withdrawal', 100.0, '',           '', 'seed split a',   -1, '2025-08-10T00:00:00', NULL, NULL, '2025-08-10T00:00:00', NULL, -1),
  (187, 12, NULL, 14, 'Withdrawal', 15.0,  '',           '', 'seed split b',   -1, '2025-07-05T00:00:00', NULL, NULL, '2025-07-05T00:00:00', NULL, -1);

INSERT INTO SPLITTRANSACTIONS_V1 (SPLITTRANSID, TRANSID, CATEGID, SPLITTRANSAMOUNT, NOTES) VALUES
  (1, 186, 30,  40.0, 'groceries'),
  (2, 186, 102, 60.0, 'subs'),
  (3, 187, 102, 10.0, 'subs'),
  (4, 187, 30,  5.0,  'groceries');
