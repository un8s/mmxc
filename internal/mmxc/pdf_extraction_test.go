package mmxc

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEncryptedOriginalPDFExtraction(t *testing.T) {
	encoded, err := os.ReadFile("testdata/owner_password_statement.pdf.b64")
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	original := append([]byte(nil), encrypted...)

	text, err := extractPDFTextResult(encrypted)
	if err != nil {
		t.Fatal(err)
	}
	normalizedText := strings.Join(strings.Fields(text), " ")
	if len(strings.Fields(text)) < 20 {
		t.Fatalf("extracted only %d words: %q", len(strings.Fields(text)), text)
	}
	for _, want := range []string{"Account Number 12345", "Coffee Shop", "Closing Balance 1395.50"} {
		if !strings.Contains(normalizedText, want) {
			t.Fatalf("extracted text missing %q:\n%s", want, text)
		}
	}
	if !bytes.Equal(encrypted, original) {
		t.Fatal("extraction modified the uploaded PDF bytes")
	}

	request, err := BuildStatementExtractionRequest(Prompts{}, "2026-06-19", "original.pdf", encrypted, []string{"Food: Dining"})
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Images) != 0 {
		t.Fatal("a text PDF should be sent as text, not as images")
	}
	if !strings.Contains(request.Prompt, "Account Number 12345") {
		t.Fatal("production extraction prompt does not contain statement text")
	}
}

func TestPDFExtractionReportsMissingPoppler(t *testing.T) {
	_, err := extractPDFTextWithCommand(context.Background(), "/nonexistent/mmxc-pdftotext", []byte("%PDF-1.4"))
	if err == nil || !strings.Contains(err.Error(), "requires Poppler's pdftotext executable") {
		t.Fatalf("missing executable error = %v", err)
	}
}

func TestPasswordProtectedPDFIsRejected(t *testing.T) {
	command := filepath.Join(t.TempDir(), "pdftotext")
	script := "#!/bin/sh\nprintf '%s\\n' 'Command Line Error: Incorrect password' >&2\nexit 1\n"
	if err := os.WriteFile(command, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	text, err := extractPDFTextWithCommand(context.Background(), command, []byte("%PDF-1.4"))
	if !errors.Is(err, errPDFPasswordRequired) {
		t.Fatalf("extractPDFTextWithCommand error = %v, want %v", err, errPDFPasswordRequired)
	}
	if text != "" {
		t.Fatalf("password-protected extraction returned text: %q", text)
	}
}
