package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/M30W-M1X/moat/internal/audit"
)

// captureStdout runs fn with os.Stdout replaced by a pipe and returns what fn
// wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = wr
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(rd)
		done <- string(b)
	}()
	fn()
	_ = wr.Close()
	os.Stdout = orig
	return <-done
}

func writeAuditFixture(t *testing.T, dir string, n int) {
	t.Helper()
	log := logrus.New()
	log.SetLevel(logrus.ErrorLevel)
	r, err := audit.New(audit.Config{Dir: dir, Metrics: audit.NopMetrics(), Log: log})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		_ = r.RecordSync(context.Background(), audit.Event{Type: audit.TypeExec, Sandbox: "sb", Token: "ci"})
	}
	_ = r.Close()
}

func TestAuditVerifyLocalDir(t *testing.T) {
	dir := t.TempDir()
	// A minimal two-event valid chain, hashes computed by hand is fragile.
	// Instead use the audit package to write a fixture.
	writeAuditFixture(t, dir, 5)
	code := runAudit([]string{"verify", "-dir", dir})
	if code != 0 {
		t.Fatalf("verify on a clean fixture exited %d", code)
	}
	// Corrupt a line and expect a non-zero exit.
	files, _ := filepath.Glob(filepath.Join(dir, "audit-*.jsonl"))
	b, _ := os.ReadFile(files[0])
	b[len(b)/2] ^= 0x20
	os.WriteFile(files[0], b, 0o600)
	if runAudit([]string{"verify", "-dir", dir}) == 0 {
		t.Fatal("verify passed a corrupted log")
	}
}

// TestAuditQueryOfflineSince checks that the offline --dir path honours
// --since. Before the fix it ignored the flag and returned the whole log.
func TestAuditQueryOfflineSince(t *testing.T) {
	dir := t.TempDir()
	writeAuditFixture(t, dir, 3)

	out := captureStdout(t, func() {
		if code := runAudit([]string{"query", "-dir", dir}); code != 0 {
			t.Fatalf("query exited %d", code)
		}
	})
	if !strings.Contains(out, "exec") {
		t.Fatalf("query without --since returned no events: %q", out)
	}

	out = captureStdout(t, func() {
		if code := runAudit([]string{"query", "-dir", dir, "-since", "2999-01-01T00:00:00Z"}); code != 0 {
			t.Fatalf("query --since exited %d", code)
		}
	})
	if strings.TrimSpace(out) != "" {
		t.Fatalf("query --since in the future returned events: %q", out)
	}
}
