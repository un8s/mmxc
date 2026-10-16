package mmxc

import (
	"strings"
	"testing"
)

func TestFormatMoneyCurrencyAware(t *testing.T) {
	cases := []struct {
		name string
		v    float64
		f    currencyFormat
		want string
	}{
		{
			name: "us dollar default",
			v:    1234567.5,
			f:    currencyFormat{Prefix: "$", Decimal: ".", Group: ",", Decimals: 2},
			want: "$1,234,567.50",
		},
		{
			name: "euro suffix with space group and comma decimal",
			v:    1234.5,
			f:    currencyFormat{Suffix: " €", Decimal: ",", Group: " ", Decimals: 2},
			want: "1 234,50 €",
		},
		{
			name: "negative keeps sign before prefix",
			v:    -42.25,
			f:    currencyFormat{Prefix: "$", Decimal: ".", Group: ",", Decimals: 2},
			want: "-$42.25",
		},
		{
			name: "zero decimals drops fraction and separator",
			v:    1500,
			f:    currencyFormat{Prefix: "¥", Decimal: ".", Group: ",", Decimals: 0},
			want: "¥1,500",
		},
		{
			name: "three decimals",
			v:    1.5,
			f:    currencyFormat{Prefix: "", Suffix: " BHD", Decimal: ".", Group: ",", Decimals: 3},
			want: "1.500 BHD",
		},
		{
			name: "no group separator",
			v:    1234567,
			f:    currencyFormat{Prefix: "$", Decimal: ".", Group: "", Decimals: 2},
			want: "$1234567.00",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatMoney(tc.v, tc.f); got != tc.want {
				t.Errorf("formatMoney(%v) = %q, want %q", tc.v, got, tc.want)
			}
		})
	}
}

func TestScaleToDecimals(t *testing.T) {
	cases := []struct {
		scale int
		want  int
	}{
		{1, 0},
		{10, 1},
		{100, 2},
		{1000, 3},
		{0, 2},            // missing/invalid -> default
		{-5, 2},           // negative -> default
		{100000000000, 2}, // absurd -> default (>8)
	}
	for _, tc := range cases {
		if got := scaleToDecimals(tc.scale); got != tc.want {
			t.Errorf("scaleToDecimals(%d) = %d, want %d", tc.scale, got, tc.want)
		}
	}
}

func TestLocalDateLayout(t *testing.T) {
	p := pageData{lang: "de"}
	if got := p.DateFmt("2026-03-09"); got != "09.03.2026" {
		t.Errorf("German DateFmt = %q, want 09.03.2026", got)
	}
	p = pageData{lang: "fr"}
	if got := p.DateFmt("2026-03-09"); got != "09/03/2026" {
		t.Errorf("French DateFmt = %q, want 09/03/2026", got)
	}
	p = pageData{lang: "en"}
	if got := p.DateFmt("2026-03-09"); got != "2026-03-09" {
		t.Errorf("English DateFmt = %q, want unchanged ISO", got)
	}
	p = pageData{lang: "sv"}
	if got := p.DateFmt("2026-03-09"); got != "2026-03-09" {
		t.Errorf("Swedish DateFmt = %q, want ISO", got)
	}
	// Unparseable input falls back to the original string.
	p = pageData{lang: "de"}
	if got := p.DateFmt("not-a-date"); got != "not-a-date" {
		t.Errorf("invalid DateFmt = %q, want passthrough", got)
	}
}

func TestParseAmountExpression(t *testing.T) {
	cases := []struct {
		in      string
		want    float64
		wantErr bool
	}{
		{in: "123.25 + 20-0.01", want: 143.24},
		{in: "12.50", want: 12.5},
		{in: "  7  ", want: 7},
		{in: ".5", want: 0.5},
		{in: "5.", want: 5},
		{in: "100-40-20", want: 40},
		{in: "0.1+0.2", want: 0.3},
		{in: "-25", want: -25},
		{in: "-25+30", want: 5},
		{in: "+8", want: 8},
		{in: "20 - 30", want: -10},
		// Spaces are insignificant, so a space-grouped number still reads correctly.
		{in: "1 000.50", want: 1000.5},
		{in: "12,50", wantErr: true},
		{in: "5*3", wantErr: true},
		{in: "5--3", wantErr: true},
		{in: "10+", wantErr: true},
		{in: "1.2.3", wantErr: true},
		{in: "1e3", wantErr: true},
		{in: "abc", wantErr: true},
		{in: "", wantErr: true},
		{in: "+", wantErr: true},
	}
	for _, tc := range cases {
		got, err := parseAmountExpression(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseAmountExpression(%q) = %v, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseAmountExpression(%q): %v", tc.in, err)
			continue
		}
		// A calculation must land exactly on the amount the user meant, with no
		// binary-summation residue left in the stored value.
		if got != tc.want {
			t.Errorf("parseAmountExpression(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestPayeeDisplay(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		bucket bool
	}{
		{in: "Grocery Mart", want: "Grocery Mart"},
		{in: "*Gas", want: "Gas", bucket: true},
		{in: "*Resto", want: "Resto", bucket: true},
		{in: "* Gas", want: "Gas", bucket: true},
		{in: "No payee", want: "No payee"},
		{in: "", want: ""},
		// A name that is only the marker keeps it rather than rendering blank.
		{in: "*", want: "*", bucket: true},
		// The marker only counts as a prefix.
		{in: "Shell*", want: "Shell*"},
	}
	for _, tc := range cases {
		if got := displayPayee(tc.in); got != tc.want {
			t.Errorf("displayPayee(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if got := isBucketPayee(tc.in); got != tc.bucket {
			t.Errorf("isBucketPayee(%q) = %v, want %v", tc.in, got, tc.bucket)
		}
	}
}

func TestPayeeHTMLEscapesAndItalicises(t *testing.T) {
	if got := string(payeeHTML("*Gas")); got != `<i class="payee-bucket">Gas</i>` {
		t.Fatalf("payeeHTML(*Gas) = %q", got)
	}
	if got := string(payeeHTML("Grocery Mart")); got != "Grocery Mart" {
		t.Fatalf("payeeHTML(Grocery Mart) = %q", got)
	}
	// payeeHTML returns template.HTML, so it must escape the name itself.
	if got := string(payeeHTML(`Ben & Jerry's <b>`)); got != `Ben &amp; Jerry&#39;s &lt;b&gt;` {
		t.Fatalf("payeeHTML did not escape a plain name: %q", got)
	}
	if got := string(payeeHTML(`*<script>alert(1)</script>`)); !strings.Contains(got, "&lt;script&gt;") || strings.Contains(got, "<script>") {
		t.Fatalf("payeeHTML did not escape a bucket name: %q", got)
	}
}

func TestListenURL(t *testing.T) {
	for addr, want := range map[string]string{
		":8080":           "http://127.0.0.1:8080",
		"0.0.0.0:8080":    "http://127.0.0.1:8080",
		"[::]:8080":       "http://127.0.0.1:8080",
		"127.0.0.1:8089":  "http://127.0.0.1:8089",
		"mmxc.lan:80":     "http://mmxc.lan:80",
		"[fd00::5]:8080":  "http://[fd00::5]:8080",
		"no-port-address": "http://no-port-address",
	} {
		if got := listenURL(addr); got != want {
			t.Errorf("listenURL(%q) = %q, want %q", addr, got, want)
		}
	}
}
