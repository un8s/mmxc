package mmxc

import (
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

func TestSignVerifyRoundTrip(t *testing.T) {
	key := []byte("test-key-0123456789012345678901234")
	payload := []byte(`{"sub":"alice","exp":123}`)

	signed := signValue(key, payload)
	got, ok := verifyValue(key, signed)
	if !ok {
		t.Fatal("verifyValue rejected a value we just signed")
	}
	if string(got) != string(payload) {
		t.Fatalf("payload = %q, want %q", got, payload)
	}
}

func TestVerifyRejectsTamperedMAC(t *testing.T) {
	key := []byte("test-key-0123456789012345678901234")
	signed := signValue(key, []byte("hello"))

	// Flip the last character of the tag.
	tampered := signed[:len(signed)-1]
	if signed[len(signed)-1] == 'A' {
		tampered += "B"
	} else {
		tampered += "A"
	}
	if _, ok := verifyValue(key, tampered); ok {
		t.Fatal("verifyValue accepted a tampered tag")
	}

	// A different key must also fail.
	if _, ok := verifyValue([]byte("other-key-000000000000000000000000"), signed); ok {
		t.Fatal("verifyValue accepted a value signed with a different key")
	}

	// Missing separator.
	if _, ok := verifyValue(key, "no-separator"); ok {
		t.Fatal("verifyValue accepted malformed input")
	}
}

func TestSanitizeReturnTo(t *testing.T) {
	cases := map[string]string{
		"":                 "/",
		"/":                "/",
		"/accounts?x=1":    "/accounts?x=1",
		"//evil.com":       "/",
		"https://evil.com": "/",
		"http://evil.com":  "/",
		"/path\\to":        "/",
		"relative":         "/",
		"/a//b":            "/a//b", // internal double slash is allowed; only leading // is rejected
	}
	for in, want := range cases {
		if got := sanitizeReturnTo(in); got != want {
			t.Errorf("sanitizeReturnTo(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPKCEPairChallenge(t *testing.T) {
	verifier, challenge, err := pkcePair()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if challenge != want {
		t.Fatalf("challenge = %q, want %q", challenge, want)
	}
}

func TestAudienceUnmarshal(t *testing.T) {
	var a audience
	if err := a.UnmarshalJSON([]byte(`"single"`)); err != nil {
		t.Fatal(err)
	}
	if len(a) != 1 || a[0] != "single" {
		t.Fatalf("string aud = %v", a)
	}
	if err := a.UnmarshalJSON([]byte(`["x","y"]`)); err != nil {
		t.Fatal(err)
	}
	if len(a) != 2 || a[1] != "y" {
		t.Fatalf("array aud = %v", a)
	}
}
