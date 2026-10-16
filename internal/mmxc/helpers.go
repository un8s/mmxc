package mmxc

import (
	"errors"
	"fmt"
	"html/template"
	"math"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// bucketPayeePrefix marks a "bucket" payee — a stand-in for a class of spending
// ("*Gas", "*Resto") rather than a real merchant. It is a naming convention, not
// schema: the prefix lives in PAYEENAME and round-trips to MMEX untouched, so it
// is only ever stripped for display. Never strip it on the way into the
// database, into a form value, into a datalist option, or into an LLM prompt —
// those all feed back into payee resolution.
const bucketPayeePrefix = "*"

func isBucketPayee(name string) bool {
	return strings.HasPrefix(strings.TrimSpace(name), bucketPayeePrefix)
}

// displayPayee drops the bucket prefix. A name that is nothing but the prefix
// keeps it, so no payee ever renders blank.
func displayPayee(name string) string {
	if !isBucketPayee(name) {
		return name
	}
	stripped := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(name), bucketPayeePrefix))
	if stripped == "" {
		return name
	}
	return stripped
}

// payeeHTML renders a payee name for display: bucket payees lose the prefix and
// are italicised, every other name renders unchanged. The name is escaped here
// because the result is template.HTML.
func payeeHTML(name string) template.HTML {
	escaped := template.HTMLEscapeString(displayPayee(name))
	if !isBucketPayee(name) {
		return template.HTML(escaped)
	}
	return template.HTML(`<i class="payee-bucket">` + escaped + `</i>`)
}

func parsePositiveInt(value string) int {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

func parseFormFloat(r *http.Request, name string, optional bool) (float64, error) {
	raw := strings.TrimSpace(r.FormValue(name))
	if raw == "" {
		if optional {
			return 0, nil
		}
		raw = "0"
	}
	return parseAmountExpression(raw)
}

// errAmountSyntax is returned for anything parseAmountExpression cannot read as
// a number or a "+"/"-" chain of numbers.
var errAmountSyntax = errors.New("must be a number or a + / - calculation")

// parseAmountExpression reads an amount field. A bare decimal number is the
// common case; on top of that it evaluates a plain chain of additions and
// subtractions so a user can type "123.25 + 20-0.01" and have it add up.
// Whitespace is insignificant, a leading sign is allowed, and there is no
// precedence to worry about because "+" and "-" are the only operators.
//
// The amount inputs are plain text fields (a number input would refuse to hand
// over an expression at all), so every amount arriving from a form goes through
// here — the browser-side evaluator in app.js is a convenience that shows the
// result early, not the authority.
func parseAmountExpression(raw string) (float64, error) {
	expr := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == ' ' {
			return -1
		}
		return r
	}, raw)
	if expr == "" {
		return 0, errAmountSyntax
	}

	total, sign, terms := 0.0, 1.0, 0
	for i := 0; i < len(expr); {
		if terms == 0 {
			// A leading sign is part of the first term rather than an operator.
			for i < len(expr) && (expr[i] == '+' || expr[i] == '-') {
				if expr[i] == '-' {
					sign = -sign
				}
				i++
			}
		} else {
			switch expr[i] {
			case '+':
				sign = 1
			case '-':
				sign = -1
			default:
				return 0, errAmountSyntax
			}
			i++
		}
		start := i
		for i < len(expr) && (expr[i] == '.' || (expr[i] >= '0' && expr[i] <= '9')) {
			i++
		}
		value, err := strconv.ParseFloat(expr[start:i], 64)
		if err != nil {
			return 0, errAmountSyntax
		}
		total += sign * value
		terms++
	}
	if terms > 1 {
		// Summing decimals in binary leaves noise well below any currency's
		// smallest unit ("123.25+20-0.01" lands on 143.24000000000001); round it
		// off so the stored amount is the one the user typed.
		total = math.Round(total*1e4) / 1e4
	}
	return total, nil
}

func parseOptionalDate(value, label string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if _, err := time.Parse("2006-01-02", value); err != nil {
		return "", fmt.Errorf("%s must be YYYY-MM-DD", label)
	}
	return value, nil
}

func formBoolDefault(r *http.Request, name string, fallback bool) bool {
	values, ok := r.Form[name]
	if !ok {
		return fallback
	}
	for _, value := range values {
		if value == "true" || value == "1" || value == "on" {
			return true
		}
	}
	return false
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func boolText(value bool) string {
	if value {
		return "TRUE"
	}
	return "FALSE"
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func humanBytes(size int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	value := float64(size)
	unit := 0
	for value >= 1024 && unit < len(units)-1 {
		value /= 1024
		unit++
	}
	if unit == 0 {
		return fmt.Sprintf("%d %s", size, units[unit])
	}
	return fmt.Sprintf("%.1f %s", value, units[unit])
}

func localReturnURL(r *http.Request) string {
	q := r.URL.Query()
	q.Del("edit_trans_id")
	q.Del("scroll")
	if encoded := q.Encode(); encoded != "" {
		return r.URL.Path + "?" + encoded
	}
	return r.URL.Path
}

func sanitizeLocalPath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") {
		return ""
	}
	if strings.Contains(value, "\n") || strings.Contains(value, "\r") {
		return ""
	}
	return value
}

func scrollParam(s string) string {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n >= 0 {
		return strconv.Itoa(n)
	}
	return ""
}

// withScroll appends a scroll offset to a local URL so a page the transaction
// editor returns to lands where the row was clicked (app.js restores it).
func withScroll(dest, scroll string) string {
	s := scrollParam(scroll)
	if s == "" {
		return dest
	}
	sep := "?"
	if strings.Contains(dest, "?") {
		sep = "&"
	}
	return dest + sep + "scroll=" + s
}

// templateDict builds a map from alternating key/value arguments so a single
// template can be parameterized at the call site (e.g. the shared
// transaction-entry / split-editor partials). Keys must be strings.
func templateDict(pairs ...any) (map[string]any, error) {
	if len(pairs)%2 != 0 {
		return nil, fmt.Errorf("dict requires an even number of arguments")
	}
	m := make(map[string]any, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		key, ok := pairs[i].(string)
		if !ok {
			return nil, fmt.Errorf("dict keys must be strings, got %T", pairs[i])
		}
		m[key] = pairs[i+1]
	}
	return m, nil
}

// sqlPlaceholders builds the "?, ?, ?" body of an IN clause for n arguments.
func sqlPlaceholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

func formatMoney(v float64, f currencyFormat) string {
	sign := ""
	if v < 0 {
		sign = "-"
		v = -v
	}
	decimals := f.Decimals
	if decimals < 0 || decimals > 8 {
		decimals = 2
	}
	s := fmt.Sprintf("%.*f", decimals, v)
	decimal := f.Decimal
	if decimal == "" {
		decimal = "."
	}
	intPart, fracPart := s, ""
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		intPart = s[:dot]
		if decimals > 0 {
			fracPart = decimal + s[dot+1:]
		}
	}
	grouped := groupThousands(intPart, f.Group)
	return sign + f.Prefix + grouped + fracPart + f.Suffix
}

// groupThousands inserts sep separators into a string of digits (the integer
// part of a formatted number), e.g. ("1234567", ",") -> "1,234,567".
func groupThousands(digits, sep string) string {
	if sep == "" {
		return digits
	}
	n := len(digits)
	if n <= 3 {
		return digits
	}
	var b strings.Builder
	lead := n % 3
	if lead > 0 {
		b.WriteString(digits[:lead])
		if n > lead {
			b.WriteString(sep)
		}
	}
	for i := lead; i < n; i += 3 {
		b.WriteString(digits[i : i+3])
		if i+3 < n {
			b.WriteString(sep)
		}
	}
	return b.String()
}

func short(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 96 {
		return s[:93] + "..."
	}
	return s
}

func isPDFName(name string) bool {
	return strings.EqualFold(filepath.Ext(name), ".pdf")
}

func formatDate() string {
	return time.Now().Format("2006-01-02")
}

// localDateLayout returns the Go time layout for displaying dates in a given
// locale. The stored/input format is always ISO (2006-01-02); this is only
// for human-readable display in table cells and prose.
func localDateLayout(lang string) string {
	switch lang {
	case "de", "nl", "tr", "cs", "pl", "hu", "ro", "fi":
		return "02.01.2006"
	case "sv", "no", "da":
		return "2006-01-02" // Scandinavian standard is ISO
	case "fr", "es", "it", "pt", "uk", "ru", "ar", "el":
		return "02/01/2006"
	case "ja", "zh", "ko":
		return "2006/01/02"
	default: // "en" and anything unrecognised
		return "2006-01-02"
	}
}

// yearMonth reduces a YYYY-MM-DD string to YYYY-MM for an <input type="month">.
func yearMonth(s string) string {
	if len(s) >= 7 {
		return s[:7]
	}
	return s
}

// yearOnly reduces a YYYY-MM-DD string to YYYY for a year picker.
func yearOnly(s string) string {
	if len(s) >= 4 {
		return s[:4]
	}
	return s
}

// listenURL is the address to open in a browser for an http_addr. A port-only
// or wildcard address (":8080", "0.0.0.0:8080", "[::]:8080") is reached on
// this machine through 127.0.0.1; a named host is used as given.
func listenURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}
