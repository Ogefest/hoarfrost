package extract

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// OCR recognizes text in a PDF or image using pdftoppm and tesseract.
// It is only run on demand; languages default to "pol+eng".
func OCR(ctx context.Context, body []byte, contentType, languages string) (string, error) {
	tesseract, err := exec.LookPath("tesseract")
	if err != nil {
		return "", ErrToolMissing
	}
	if languages == "" {
		languages = "pol+eng"
	}
	dir, err := os.MkdirTemp("", "hoarfrost-ocr-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)

	var images []string
	if strings.HasPrefix(contentType, "application/pdf") {
		pdftoppm, err := exec.LookPath("pdftoppm")
		if err != nil {
			return "", ErrToolMissing
		}
		in := filepath.Join(dir, "in.pdf")
		if err := os.WriteFile(in, body, 0o600); err != nil {
			return "", err
		}
		if out, err := exec.CommandContext(ctx, pdftoppm, "-r", "300", "-png", in, filepath.Join(dir, "page")).CombinedOutput(); err != nil {
			return "", &ToolError{Tool: "pdftoppm", Output: string(out), Err: err}
		}
		images, _ = filepath.Glob(filepath.Join(dir, "page*.png"))
		sort.Strings(images)
	} else {
		in := filepath.Join(dir, "image")
		if err := os.WriteFile(in, body, 0o600); err != nil {
			return "", err
		}
		images = []string{in}
	}
	var text bytes.Buffer
	for _, img := range images {
		cmd := exec.CommandContext(ctx, tesseract, img, "stdout", "-l", languages)
		out, err := cmd.Output()
		if err != nil {
			return "", &ToolError{Tool: "tesseract", Err: err}
		}
		text.Write(out)
		text.WriteByte('\n')
	}
	return normalizeSpace(text.String()), nil
}

// ToolError wraps a failure of an external program.
type ToolError struct {
	Tool   string
	Output string
	Err    error
}

func (e *ToolError) Error() string {
	return "extract: " + e.Tool + ": " + e.Err.Error() + " " + strings.TrimSpace(e.Output)
}

func (e *ToolError) Unwrap() error { return e.Err }
