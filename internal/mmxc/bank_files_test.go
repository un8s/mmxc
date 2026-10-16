package mmxc

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A synthetic OFX 1.x (SGML) bank download: leaf elements have no closing
// tags, the empty <MEMO> must not swallow the rest of its transaction, and the
// file is Windows-1252, as its header says.
var sampleOFXSGML = "OFXHEADER:100\r\nDATA:OFXSGML\r\nVERSION:102\r\nSECURITY:NONE\r\nENCODING:USASCII\r\nCHARSET:1252\r\nCOMPRESSION:NONE\r\nOLDFILEUID:NONE\r\nNEWFILEUID:NONE\r\n\r\n" +
	"<OFX><SIGNONMSGSRSV1><SONRS><STATUS><CODE>0<SEVERITY>INFO</STATUS><DTSERVER>20251031120000[-5:EST]<LANGUAGE>ENG<FI><ORG>Harbor Trust<FID>12345</FI></SONRS></SIGNONMSGSRSV1>" +
	"<BANKMSGSRSV1><STMTTRNRS><TRNUID>1<STATUS><CODE>0<SEVERITY>INFO</STATUS><STMTRS><CURDEF>USD" +
	"<BANKACCTFROM><BANKID>0001<ACCTID>3000114821<ACCTTYPE>CHECKING</BANKACCTFROM>" +
	"<BANKTRANLIST><DTSTART>20251001<DTEND>20251031" +
	"<STMTTRN><TRNTYPE>POS<DTPOSTED>20251003120000.000[-5:EST]<TRNAMT>-84.20<FITID>A1<NAME>GREENLEAF GROCERS #22<MEMO></STMTTRN>" +
	"<STMTTRN><TRNTYPE>CREDIT<DTPOSTED>20251015<TRNAMT>2,150.00<FITID>A2<NAME>PAYROLL<MEMO>NORTHWIND LTD</STMTTRN>" +
	"<STMTTRN><TRNTYPE>CHECK<DTPOSTED>20251020<TRNAMT>-45.00<FITID>A3<CHECKNUM>107</STMTTRN>" +
	"<STMTTRN><TRNTYPE>POS<DTPOSTED>20251022<TRNAMT>-12.50<FITID>A4<NAME>CAF\xc9 FERN &amp; CO<MEMO>CAF\xc9 FERN &amp; CO</STMTTRN>" +
	"<STMTTRN><TRNTYPE>OTHER<DTPOSTED>20251023<TRNAMT>0.00<FITID>A5<NAME>NOTICE</STMTTRN>" +
	"</BANKTRANLIST><LEDGERBAL><BALAMT>3007.30<DTASOF>20251031</LEDGERBAL></STMTRS></STMTTRNRS></BANKMSGSRSV1></OFX>\r\n"

// A synthetic OFX 2.x (XML) card download whose balance is dated after the
// last day it covers.
const sampleOFXXML = `<?xml version="1.0" encoding="UTF-8" standalone="no"?>
<?OFX OFXHEADER="200" VERSION="211" SECURITY="NONE" OLDFILEUID="NONE" NEWFILEUID="NONE"?>
<OFX>
  <CREDITCARDMSGSRSV1><CCSTMTTRNRS><TRNUID>1</TRNUID>
    <CCSTMTRS>
      <CURDEF>USD</CURDEF>
      <CCACCTFROM><ACCTID>5191230000009876</ACCTID></CCACCTFROM>
      <BANKTRANLIST>
        <STMTTRN><TRNTYPE>DEBIT</TRNTYPE><DTPOSTED>20250905</DTPOSTED><TRNAMT>-63.10</TRNAMT><FITID>C1</FITID><NAME>PINE STREET PHARMACY</NAME><MEMO></MEMO></STMTTRN>
        <STMTTRN><TRNTYPE>CREDIT</TRNTYPE><DTPOSTED>20250912</DTPOSTED><TRNAMT>200.00</TRNAMT><FITID>C2</FITID><NAME>PAYMENT - THANK YOU</NAME></STMTTRN>
      </BANKTRANLIST>
      <LEDGERBAL><BALAMT>-412.55</BALAMT><DTASOF>20251001</DTASOF></LEDGERBAL>
    </CCSTMTRS>
  </CCSTMTTRNRS></CREDITCARDMSGSRSV1>
</OFX>`

func TestParseOFXSGMLBankStatement(t *testing.T) {
	sections, ok, err := parseBankFile("download.ofx", []byte(sampleOFXSGML))
	if !ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if len(sections) != 1 {
		t.Fatalf("sections = %d, want 1", len(sections))
	}
	h := sections[0].Header
	want := statementHeader{
		AccountName: "Harbor Trust Checking", AccountNumber: "3000114821",
		PeriodStart: "2025-10-01", PeriodEnd: "2025-10-31",
		ClosingDate: "2025-10-31", ClosingBalance: 3007.30, HasClosing: true,
	}
	if h != want {
		t.Fatalf("header = %+v\nwant     %+v", h, want)
	}
	got := sections[0].Candidates
	wantRows := []importCandidate{
		{Date: "2025-10-03", Type: "Withdrawal", Amount: 84.20, Description: "GREENLEAF GROCERS #22", Payee: "GREENLEAF GROCERS #22"},
		{Date: "2025-10-15", Type: "Deposit", Amount: 2150, Description: "PAYROLL NORTHWIND LTD", Payee: "PAYROLL"},
		{Date: "2025-10-20", Type: "Withdrawal", Amount: 45, Description: "Cheque 107", Payee: "Cheque 107"},
		{Date: "2025-10-22", Type: "Withdrawal", Amount: 12.50, Description: "CAFÉ FERN & CO", Payee: "CAFÉ FERN & CO"},
	}
	if len(got) != len(wantRows) {
		t.Fatalf("candidates = %+v, want %d rows (the zero row dropped)", got, len(wantRows))
	}
	for i, w := range wantRows {
		g := got[i]
		if g.Date != w.Date || g.Type != w.Type || g.Amount != w.Amount || g.Description != w.Description || g.Payee != w.Payee || g.Category != "" {
			t.Errorf("row %d = %+v\nwant   %+v", i, g, w)
		}
	}
}

func TestParseOFXXMLCardStatement(t *testing.T) {
	sections, ok, err := parseBankFile("export.qfx", []byte(sampleOFXXML))
	if !ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	h := sections[0].Header
	if h.AccountName != "Credit Card" || h.AccountNumber != "5191230000009876" {
		t.Fatalf("account = %q %q", h.AccountName, h.AccountNumber)
	}
	// No BANKTRANLIST dates: the period is the rows' range.
	if h.PeriodStart != "2025-09-05" || h.PeriodEnd != "2025-09-12" {
		t.Fatalf("period = %s..%s", h.PeriodStart, h.PeriodEnd)
	}
	// The balance is dated after the period, so it cannot check these rows.
	if h.HasClosing {
		t.Fatalf("closing balance kept although dated after the period: %+v", h)
	}
	rows := sections[0].Candidates
	if len(rows) != 2 || rows[0].Type != "Withdrawal" || rows[0].Amount != 63.10 || rows[1].Type != "Deposit" || rows[1].Amount != 200 {
		t.Fatalf("rows = %+v", rows)
	}

	// Dated within the period, a card balance is held as the statement prints
	// it: the amount owed, positive.
	inPeriod := strings.Replace(sampleOFXXML, "<DTASOF>20251001", "<DTASOF>20250912", 1)
	sections, _, err = parseBankFile("export.qfx", []byte(inPeriod))
	if err != nil {
		t.Fatal(err)
	}
	if h := sections[0].Header; !h.HasClosing || h.ClosingBalance != 412.55 || h.ClosingDate != "2025-09-12" {
		t.Fatalf("card closing = %+v", h)
	}
}

func TestParseOFXWithoutStatement(t *testing.T) {
	_, ok, err := parseBankFile("holdings.ofx", []byte("<OFX><INVSTMTMSGSRSV1></INVSTMTMSGSRSV1></OFX>"))
	if !ok || err == nil || !strings.Contains(err.Error(), "no bank or credit card statement") {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}

func TestParseQIFDayFirstWindows1252(t *testing.T) {
	qif := "!Type:Bank\r\n" +
		"D03.10.2025\r\nT-1.234,56\r\nPM\xfchlenweg Miete\r\nMOktober\r\nLWohnen:Miete\r\n^\r\n" +
		"D15.10.2025\r\nT2.500,00\r\nPGehalt Nordwind GmbH\r\n^\r\n" +
		"D20.10.2025\r\nT-9,99\r\nPStreamdienst\r\nLFreizeit:Abos/Privat\r\n^\r\n"
	sections, ok, err := parseBankFile("umsaetze.qif", []byte(qif))
	if !ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	h := sections[0].Header
	if h.PeriodStart != "2025-10-03" || h.PeriodEnd != "2025-10-20" || h.HasClosing {
		t.Fatalf("header = %+v", h)
	}
	rows := sections[0].Candidates
	want := []importCandidate{
		{Date: "2025-10-03", Type: "Withdrawal", Amount: 1234.56, Description: "Mühlenweg Miete Oktober", Payee: "Mühlenweg Miete", Category: "Wohnen: Miete"},
		{Date: "2025-10-15", Type: "Deposit", Amount: 2500, Description: "Gehalt Nordwind GmbH", Payee: "Gehalt Nordwind GmbH"},
		{Date: "2025-10-20", Type: "Withdrawal", Amount: 9.99, Description: "Streamdienst", Payee: "Streamdienst", Category: "Freizeit: Abos"},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %+v", rows)
	}
	for i, w := range want {
		g := rows[i]
		if g.Date != w.Date || g.Type != w.Type || g.Amount != w.Amount || g.Description != w.Description || g.Payee != w.Payee || g.Category != w.Category {
			t.Errorf("row %d = %+v\nwant   %+v", i, g, w)
		}
	}
}

func TestParseQIFAccountsAndDateOrder(t *testing.T) {
	// A Quicken export with two accounts, US dates (decided by 10/23) and
	// Quicken's apostrophe years; the opening-balance record is skipped.
	qif := "!Account\nNHarbor Chequing\nTBank\n^\n!Type:Bank\n" +
		"D10/ 1'25\nT0.00\nPOpening Balance\nL[Harbor Chequing]\n^\n" +
		"D10/ 2'25\nT-40.00\nPFern Cafe\n^\n" +
		"D10/23'25\nT-1,050.00\nPCity Mortgage\nL[Mortgage]\n^\n" +
		"!Account\nNHarbor Card\nTCCard\n^\n!Type:CCard\n" +
		"D10/05/2025\nT-15.25\nPPine Street Pharmacy\n^\n"
	sections, _, err := parseBankFile("quicken.qif", []byte(qif))
	if err != nil {
		t.Fatal(err)
	}
	if len(sections) != 2 {
		t.Fatalf("sections = %+v", sections)
	}
	if sections[0].Header.AccountName != "Harbor Chequing" || sections[1].Header.AccountName != "Harbor Card" {
		t.Fatalf("account names = %q, %q", sections[0].Header.AccountName, sections[1].Header.AccountName)
	}
	chequing := sections[0].Candidates
	if len(chequing) != 2 || chequing[0].Date != "2025-10-02" || chequing[1].Date != "2025-10-23" || chequing[1].Amount != 1050 || chequing[1].Category != "" {
		t.Fatalf("chequing rows = %+v", chequing)
	}
	if card := sections[1].Candidates; len(card) != 1 || card[0].Date != "2025-10-05" || card[0].Type != "Withdrawal" {
		t.Fatalf("card rows = %+v", card)
	}

	if _, _, err := parseBankFile("mixed.qif", []byte("!Type:Bank\nD13/01/2025\nT-1\nPA\n^\nD01/13/2025\nT-1\nPB\n^\n")); err == nil {
		t.Fatal("mixed day-first and month-first dates should be rejected")
	}
}

func TestParseBankFileLeavesOtherFilesToTheModel(t *testing.T) {
	for name, data := range map[string]string{
		"statement.pdf": "%PDF-1.4 synthetic",
		"statement.txt": "Everyday Chequing statement for October 2025 with transactions",
		"export.csv":    "Date,Description,Amount\n2025-10-03,GREENLEAF GROCERS,-84.20\n",
	} {
		if _, ok, _ := parseBankFile(name, []byte(data)); ok {
			t.Errorf("%s was taken for a bank download", name)
		}
	}
}

// TestExtractStatementUploadParsesOFXWithoutAI runs a download through the
// upload path with no AI endpoint configured: it is read and archived as an
// extracted document rather than merely attached.
func TestExtractStatementUploadParsesOFXWithoutAI(t *testing.T) {
	a, err := newApp(config{DBPath: fixtureDB(t), BackupDir: "backups", StatementDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("month", "2025-10"); err != nil {
		t.Fatal(err)
	}
	part, err := mw.CreateFormFile("statement", "download.ofx")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte(sampleOFXSGML)); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/review/extract", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if err := req.ParseMultipartForm(32 << 20); err != nil {
		t.Fatal(err)
	}

	res, err := a.extractStatementUpload(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.DocumentHash == "" || len(res.Candidates) != 4 || res.Month != "2025-10" {
		t.Fatalf("result document=%q candidates=%d month=%q", res.DocumentHash, len(res.Candidates), res.Month)
	}
	if !res.Header.HasClosing || res.Header.ClosingBalance != 3007.30 {
		t.Fatalf("header = %+v", res.Header)
	}
	doc, err := a.loadStatementDocument(res.DocumentHash)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Sections) != 1 || len(doc.Sections[0].Candidates) != 4 {
		t.Fatalf("archived document = %+v", doc)
	}

	// A broken download is reported, not handed to the model as text.
	if _, ok, err := parseBankFile("broken.ofx", []byte("not an ofx file")); !ok || err == nil {
		t.Fatalf("broken OFX: ok=%v err=%v", ok, err)
	}
}
