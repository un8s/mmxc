package mmxc

import (
	"bytes"
	"errors"
	"fmt"
	"html"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Bank transaction downloads in a format with a published definition — OFX
// (offered as "Microsoft Money"; Quicken's QFX is OFX with extra Intuit tags)
// and QIF ("Quicken") — are read straight into statement sections, with no
// model involved: the amounts are exact, any length works, and import keeps
// working without an AI endpoint. Payee and category then go through the same
// learned rules, history and name matching as an extracted statement.

// parseBankFile reports ok=false when data is neither OFX nor QIF, so the
// caller falls through to model extraction. A file that is one of them but
// cannot be read returns ok=true with the error, rather than being handed to
// the model as text.
func parseBankFile(filename string, data []byte) (sections []statementSection, ok bool, err error) {
	text := decodeBankText(data)
	ext := strings.ToLower(filepath.Ext(filename))
	head := strings.ToUpper(strings.TrimSpace(text))
	if len(head) > 4096 {
		head = head[:4096]
	}
	switch {
	case ext == ".ofx" || ext == ".qfx" || strings.HasPrefix(head, "OFXHEADER:") || strings.Contains(head, "<OFX>"):
		sections, err = parseOFX(text)
		return sections, true, err
	case ext == ".qif" || strings.HasPrefix(head, "!TYPE:") || strings.HasPrefix(head, "!ACCOUNT") || strings.HasPrefix(head, "!OPTION:"):
		sections, err = parseQIF(text)
		return sections, true, err
	}
	return nil, false, nil
}

// decodeBankText returns data as UTF-8. Downloads that are not valid UTF-8 are
// read as Windows-1252, the code page OFX 1.x files declare (CHARSET:1252) and
// most QIF exports use; for ASCII files the two agree anyway.
func decodeBankText(data []byte) string {
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	if utf8.Valid(data) {
		return string(data)
	}
	var b strings.Builder
	b.Grow(len(data))
	for _, c := range data {
		if c >= 0x80 && c <= 0x9f && cp1252High[c-0x80] != 0 {
			b.WriteRune(cp1252High[c-0x80])
		} else {
			b.WriteRune(rune(c))
		}
	}
	return b.String()
}

// cp1252High maps Windows-1252 bytes 0x80–0x9F, where it differs from
// Latin-1; 0 marks the five bytes it leaves undefined.
var cp1252High = [32]rune{
	'€', 0, '‚', 'ƒ', '„', '…', '†', '‡', 'ˆ', '‰', 'Š', '‹', 'Œ', 0, 'Ž', 0,
	0, '‘', '’', '“', '”', '•', '–', '—', '˜', '™', 'š', '›', 'œ', 0, 'ž', 'Ÿ',
}

// ofxNode is one element of an OFX document. Leaf elements carry Value.
type ofxNode struct {
	Name     string
	Value    string
	Children []*ofxNode
}

// parseOFXTree reads both OFX dialects into one tree: OFX 1.x is SGML, where
// leaf elements have no closing tag, and OFX 2.x is XML, where they do. An
// element followed by text is a leaf; anything else opens an aggregate, which
// a closing tag pops, together with anything still open inside it. The header
// before <OFX> (key:value lines in 1.x, processing instructions in 2.x) is
// skipped.
func parseOFXTree(text string) (*ofxNode, error) {
	start := strings.Index(strings.ToUpper(text), "<OFX>")
	if start < 0 {
		return nil, errors.New("no <OFX> element")
	}
	root := &ofxNode{}
	stack := []*ofxNode{root}
	rest := text[start:]
	for {
		open := strings.IndexByte(rest, '<')
		if open < 0 {
			break
		}
		closeAt := strings.IndexByte(rest[open:], '>')
		if closeAt < 0 {
			break
		}
		tag := strings.TrimSpace(rest[open+1 : open+closeAt])
		rest = rest[open+closeAt+1:]
		if tag == "" || tag[0] == '?' || tag[0] == '!' {
			continue
		}
		if tag[0] == '/' {
			name := strings.ToUpper(strings.TrimSpace(tag[1:]))
			for i := len(stack) - 1; i > 0; i-- {
				if stack[i].Name == name {
					stack = stack[:i]
					break
				}
			}
			continue
		}
		selfClosing := strings.HasSuffix(tag, "/")
		if fields := strings.Fields(strings.TrimSuffix(tag, "/")); len(fields) > 0 {
			tag = fields[0]
		}
		node := &ofxNode{Name: strings.ToUpper(tag)}
		parent := stack[len(stack)-1]
		parent.Children = append(parent.Children, node)
		next := strings.IndexByte(rest, '<')
		if next < 0 {
			next = len(rest)
		}
		if value := strings.TrimSpace(rest[:next]); value != "" {
			node.Value = html.UnescapeString(value)
		} else if !selfClosing {
			stack = append(stack, node)
		}
	}
	if len(root.Children) == 0 {
		return nil, errors.New("empty <OFX> element")
	}
	return root.Children[0], nil
}

// find returns the first descendant with the given name, depth-first. An
// empty SGML leaf (<MEMO> with no text) is read as an aggregate that swallows
// the elements after it, so lookups search descendants, not just children.
func (n *ofxNode) find(name string) *ofxNode {
	if n == nil {
		return nil
	}
	for _, c := range n.Children {
		if c.Name == name {
			return c
		}
		if found := c.find(name); found != nil {
			return found
		}
	}
	return nil
}

func (n *ofxNode) findAll(name string, out []*ofxNode) []*ofxNode {
	if n == nil {
		return out
	}
	for _, c := range n.Children {
		if c.Name == name {
			out = append(out, c)
		}
		out = c.findAll(name, out)
	}
	return out
}

// value returns the text of the first descendant with the given name.
func (n *ofxNode) value(name string) string {
	if found := n.find(name); found != nil {
		return found.Value
	}
	return ""
}

// ofxAccountTypes names the OFX ACCTTYPE values for display.
var ofxAccountTypes = map[string]string{
	"CHECKING":   "Checking",
	"SAVINGS":    "Savings",
	"MONEYMRKT":  "Money Market",
	"CREDITLINE": "Credit Line",
	"CD":         "Certificate of Deposit",
}

// parseOFX turns each bank (STMTRS) and credit-card (CCSTMTRS) statement in
// the file into one section. Investment statements are not read.
func parseOFX(text string) ([]statementSection, error) {
	root, err := parseOFXTree(text)
	if err != nil {
		return nil, fmt.Errorf("read OFX file: %w", err)
	}
	org := root.find("SIGNONMSGSRSV1").value("ORG")
	var sections []statementSection
	for _, kind := range []string{"STMTRS", "CCSTMTRS"} {
		for _, stmt := range root.findAll(kind, nil) {
			sections = append(sections, ofxSection(stmt, kind == "CCSTMTRS", org))
		}
	}
	if len(sections) == 0 {
		return nil, errors.New("the OFX file contains no bank or credit card statement")
	}
	return sections, nil
}

func ofxSection(stmt *ofxNode, creditCard bool, org string) statementSection {
	var header statementHeader
	label := "Credit Card"
	if creditCard {
		header.AccountNumber = stmt.find("CCACCTFROM").value("ACCTID")
	} else {
		from := stmt.find("BANKACCTFROM")
		header.AccountNumber = from.value("ACCTID")
		label = ofxAccountTypes[strings.ToUpper(from.value("ACCTTYPE"))]
	}
	header.AccountName = strings.TrimSpace(org + " " + label)

	list := stmt.find("BANKTRANLIST")
	header.PeriodStart = ofxDate(list.value("DTSTART"))
	header.PeriodEnd = ofxDate(list.value("DTEND"))
	var candidates []importCandidate
	for _, trn := range list.findAll("STMTTRN", nil) {
		if c, ok := ofxCandidate(trn); ok {
			candidates = append(candidates, c)
		}
	}
	fillPeriodFromCandidates(&header, candidates)

	// LEDGERBAL is the balance at DTASOF, usually the moment of the download.
	// It only checks this file when nothing can have been booked between the
	// last day the file covers and that moment, so a balance dated later is
	// left out rather than reported as a mismatch.
	if bal := stmt.find("LEDGERBAL"); bal != nil {
		amount, ok := parseStatementAmount(bal.value("BALAMT"))
		asOf := ofxDate(bal.value("DTASOF"))
		if ok && asOf != "" && (header.PeriodEnd == "" || asOf <= header.PeriodEnd) {
			// OFX signs balances from the holder's side; a card statement prints
			// the amount owed as a positive number, which is what the header holds.
			if creditCard {
				amount = -amount
			}
			header.ClosingDate = asOf
			header.ClosingBalance = amount
			header.HasClosing = true
		}
	}
	return statementSection{Header: header, Candidates: candidates}
}

func ofxCandidate(trn *ofxNode) (importCandidate, bool) {
	amount, ok := parseStatementAmount(trn.value("TRNAMT"))
	date := ofxDate(trn.value("DTPOSTED"))
	if !ok || amount == 0 || date == "" {
		return importCandidate{}, false
	}
	name := trn.value("NAME")
	if name == "" {
		name = trn.find("PAYEE").value("NAME")
	}
	if name == "" && trn.value("CHECKNUM") != "" {
		name = "Cheque " + trn.value("CHECKNUM")
	}
	return bankFileCandidate(date, amount, name, trn.value("MEMO"), ""), true
}

// ofxDate reads the date part of an OFX datetime (YYYYMMDD[HHMMSS[.XXX]][TZ]).
func ofxDate(v string) string {
	v = strings.TrimSpace(v)
	if len(v) < 8 {
		return ""
	}
	return normalizeISODate(v[:4] + "-" + v[4:6] + "-" + v[6:8])
}

// bankFileCandidate builds a candidate from the fields every download format
// has. The description keeps the memo when it adds something, because banks
// put the merchant in either field and cut long names over into the memo.
func bankFileCandidate(date string, amount float64, name, memo, category string) importCandidate {
	name = strings.Join(strings.Fields(name), " ")
	memo = strings.Join(strings.Fields(memo), " ")
	description := name
	lowerName, lowerMemo := strings.ToLower(name), strings.ToLower(memo)
	switch {
	case memo == "" || strings.Contains(lowerName, lowerMemo):
	case name == "" || strings.Contains(lowerMemo, lowerName):
		description = memo
	default:
		description = name + " " + memo
	}
	payee := name
	if payee == "" {
		payee = memo
	}
	c := importCandidate{
		Date:        date,
		Type:        "Deposit",
		Amount:      amount,
		Description: description,
		Payee:       payee,
		Category:    category,
	}
	if amount < 0 {
		c.Type = "Withdrawal"
		c.Amount = -amount
	}
	return c
}

// fillPeriodFromCandidates covers the dated rows when the file states no
// period of its own.
func fillPeriodFromCandidates(header *statementHeader, candidates []importCandidate) {
	first, last := "", ""
	for _, c := range candidates {
		if first == "" || c.Date < first {
			first = c.Date
		}
		last = max(last, c.Date)
	}
	if header.PeriodStart == "" {
		header.PeriodStart = first
	}
	if header.PeriodEnd == "" {
		header.PeriodEnd = last
	}
}

// qifRecord is one transaction of a QIF file, its date still unparsed.
type qifRecord struct {
	Date, Amount, Payee, Memo, Number, Category string
}

// qifSupportedTypes are the QIF account types that hold plain transactions.
// Investment and list sections (!Type:Invst, !Type:Cat, …) are skipped.
var qifSupportedTypes = map[string]bool{"bank": true, "ccard": true, "cash": true, "oth a": true, "oth l": true}

// parseQIF reads a QIF file, one section per account. A bank download holds a
// single !Type block; a full Quicken export names each account in an !Account
// record before its transactions.
func parseQIF(text string) ([]statementSection, error) {
	type account struct {
		name    string
		records []qifRecord
	}
	var accounts []*account
	current := &account{}
	accounts = append(accounts, current)
	mode := ""
	var rec qifRecord
	var accountName string
	for _, raw := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line := strings.TrimRight(raw, " \t\r")
		if line == "" {
			continue
		}
		if line[0] == '!' {
			header := strings.ToLower(strings.TrimSpace(line[1:]))
			switch {
			case header == "account":
				mode = "account"
			case strings.HasPrefix(header, "type:"):
				mode = "skip"
				if qifSupportedTypes[strings.TrimSpace(strings.TrimPrefix(header, "type:"))] {
					mode = "transactions"
				}
			}
			continue
		}
		code, value := line[0], strings.TrimSpace(line[1:])
		switch mode {
		case "account":
			switch code {
			case 'N':
				accountName = value
			case '^':
				if len(current.records) > 0 || current.name != "" {
					current = &account{}
					accounts = append(accounts, current)
				}
				current.name = accountName
				accountName = ""
			}
		case "transactions":
			switch code {
			case 'D':
				rec.Date = value
			case 'T', 'U':
				if rec.Amount == "" || code == 'T' {
					rec.Amount = value
				}
			case 'P':
				rec.Payee = value
			case 'M':
				rec.Memo = value
			case 'N':
				rec.Number = value
			case 'L':
				rec.Category = value
			case '^':
				if rec != (qifRecord{}) {
					current.records = append(current.records, rec)
				}
				rec = qifRecord{}
			}
		}
	}
	if rec != (qifRecord{}) && mode == "transactions" {
		current.records = append(current.records, rec)
	}

	var all []qifRecord
	for _, acct := range accounts {
		all = append(all, acct.records...)
	}
	dayFirst, err := qifDayFirst(all)
	if err != nil {
		return nil, err
	}
	var sections []statementSection
	for _, acct := range accounts {
		if len(acct.records) == 0 {
			continue
		}
		var candidates []importCandidate
		for _, r := range acct.records {
			c, ok, err := qifCandidate(r, dayFirst)
			if err != nil {
				return nil, err
			}
			if ok {
				candidates = append(candidates, c)
			}
		}
		header := statementHeader{AccountName: acct.name}
		fillPeriodFromCandidates(&header, candidates)
		sections = append(sections, statementSection{Header: header, Candidates: candidates})
	}
	if len(sections) == 0 {
		return nil, errors.New("the QIF file contains no bank or credit card transactions")
	}
	return sections, nil
}

func qifCandidate(r qifRecord, dayFirst bool) (importCandidate, bool, error) {
	date, err := qifDate(r.Date, dayFirst)
	if err != nil {
		return importCandidate{}, false, err
	}
	amount, ok := parseStatementAmount(r.Amount)
	if !ok {
		return importCandidate{}, false, fmt.Errorf("the QIF file has an unreadable amount %q", r.Amount)
	}
	// "[Account]" is a transfer, which MMXC infers from rules and history
	// instead; Quicken's own opening-balance record is not a transaction.
	category := r.Category
	if strings.HasPrefix(category, "[") {
		if strings.EqualFold(r.Payee, "Opening Balance") {
			return importCandidate{}, false, nil
		}
		category = ""
	}
	if amount == 0 {
		return importCandidate{}, false, nil
	}
	name := r.Payee
	if name == "" && r.Number != "" {
		name = "Cheque " + r.Number
	}
	return bankFileCandidate(date, amount, name, r.Memo, qifCategory(category)), true, nil
}

// qifCategory turns Quicken's "Parent:Child/Class" into the "Parent: Child"
// form MMXC resolves.
func qifCategory(v string) string {
	if i := strings.IndexByte(v, '/'); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ":")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return strings.Trim(strings.Join(parts, ": "), ": ")
}

var qifDateParts = regexp.MustCompile(`^(\d{1,4})\s*[/.\-']\s*(\d{1,2})\s*[/.\-']\s*(\d{1,4})$`)

// qifDayFirst settles the day/month order once for the whole file: QIF has no
// fixed date format, and US exports write month first while European ones
// write the day first. A first field above 12 can only be a day and a second
// one only a month; when no date decides it, dotted dates are read the
// European way and others the US way.
func qifDayFirst(records []qifRecord) (bool, error) {
	dayFirst, monthFirst, dotted := false, false, false
	for _, r := range records {
		m := qifDateParts.FindStringSubmatch(strings.TrimSpace(r.Date))
		if m == nil || len(m[1]) == 4 {
			continue
		}
		first, _ := strconv.Atoi(m[1])
		second, _ := strconv.Atoi(m[2])
		dayFirst = dayFirst || first > 12
		monthFirst = monthFirst || second > 12
		dotted = dotted || strings.Contains(r.Date, ".")
	}
	if dayFirst && monthFirst {
		return false, errors.New("the QIF file mixes day-first and month-first dates")
	}
	if dayFirst || monthFirst {
		return dayFirst, nil
	}
	return dotted, nil
}

// qifDate reads 12/31/2025, 12/31'25, 31.12.2025 or 2025-12-31. A two-digit
// year is in this century up to 69 and in the last one from 70 on.
func qifDate(v string, dayFirst bool) (string, error) {
	m := qifDateParts.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return "", fmt.Errorf("the QIF file has an unreadable date %q", v)
	}
	a, _ := strconv.Atoi(m[1])
	b, _ := strconv.Atoi(m[2])
	c, _ := strconv.Atoi(m[3])
	year, month, day := c, a, b
	switch {
	case len(m[1]) == 4:
		year, month, day = a, b, c
	case dayFirst:
		month, day = b, a
	}
	if year < 100 {
		year += 2000
		if year >= 2070 {
			year -= 100
		}
	}
	date := normalizeISODate(fmt.Sprintf("%04d-%02d-%02d", year, month, day))
	if date == "" {
		return "", fmt.Errorf("the QIF file has an unreadable date %q", v)
	}
	return date, nil
}
