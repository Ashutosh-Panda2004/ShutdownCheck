package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWriteFileDoesNotTruncateExistingReportOnRenderFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(path, []byte("previous complete report"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	wantErr := errors.New("renderer failed")
	err := writeFile(path, func(w io.Writer) error {
		_, _ = io.WriteString(w, "partial")
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want renderer failure", err)
	}

	got, err := os.ReadFile(path) // #nosec G304 -- test-controlled temp path
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "previous complete report" {
		t.Errorf("existing report was damaged: %q", got)
	}
}

func TestWriteFilePublishesCompleteRestrictiveFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	if err := writeFile(path, func(w io.Writer) error {
		_, err := io.WriteString(w, "complete")
		return err
	}); err != nil {
		t.Fatalf("writeFile: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Windows ACLs do not map to POSIX permission bits; Chmod only toggles the
	// read-only attribute there.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("permissions = %o, want 600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path) // #nosec G304 -- test-controlled temp path
	if err != nil || string(data) != "complete" {
		t.Errorf("content = %q, err = %v", data, err)
	}
}

func TestWriteFileAtomicallyReplacesExistingReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(path, []byte("old complete report"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := writeFile(path, func(w io.Writer) error {
		_, err := io.WriteString(w, "new complete report")
		return err
	}); err != nil {
		t.Fatalf("writeFile: %v", err)
	}

	data, err := os.ReadFile(path) // #nosec G304 -- test-controlled temp path
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "new complete report" {
		t.Errorf("content = %q", data)
	}
}

func TestOutputAndBadgeCannotShareAPath(t *testing.T) {
	flags := resolveRunFlags(t,
		"--url", "http://localhost:8080/",
		"--pid", "4242",
		"--output", "result.json",
		"--badge", filepath.Join(".", "result.json"),
	)
	if _, err := flags.resolve(); err == nil {
		t.Fatal("one path for report and badge was accepted")
	}
}
