package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/M30W-M1X/moat/internal/audit"
)

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
