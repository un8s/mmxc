package mmxc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

var errPDFPasswordRequired = errors.New("PDF requires an open password; password-protected statement PDFs are not supported")

const pdfTextExtractionTimeout = 30 * time.Second

func extractPDFText(data []byte) string {
	text, _ := extractPDFTextResult(data)
	return text
}

func extractPDFTextResult(data []byte) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), pdfTextExtractionTimeout)
	defer cancel()
	return extractPDFTextWithCommand(ctx, "pdftotext", data)
}

// extractPDFTextWithCommand uses Poppler's pdftotext as MMXC's sole PDF text
// extractor. In particular, Poppler correctly applies ToUnicode maps for custom
// Type 3 fonts that simpler Go PDF readers commonly decode as mojibake. Passing
// the document over stdin avoids temporary files and invoking the executable
// directly avoids a shell.
func extractPDFTextWithCommand(ctx context.Context, command string, data []byte) (string, error) {
	cmd := exec.CommandContext(ctx, command, "-layout", "-enc", "UTF-8", "-nopgbrk", "-", "-")
	cmd.Stdin = bytes.NewReader(data)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("pdftotext timed out after %s", pdfTextExtractionTimeout)
		}
		var execErr *exec.Error
		if errors.As(err, &execErr) || errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("PDF statement extraction requires Poppler's pdftotext executable: %w", err)
		}
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			if strings.Contains(strings.ToLower(detail), "incorrect password") {
				return "", errPDFPasswordRequired
			}
			return "", fmt.Errorf("pdftotext failed: %s", detail)
		}
		return "", fmt.Errorf("pdftotext failed: %w", err)
	}
	return strings.TrimSpace(stdout.String()), nil
}
