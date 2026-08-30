package server

import (
	"testing"

	"github.com/M30W-M1X/moat/internal/audit"
	"github.com/sirupsen/logrus"
)

func TestNewBuildsDisabledRecorderWhenNil(t *testing.T) {
	log := logrus.New()
	log.SetLevel(logrus.ErrorLevel)
	s, err := New(Config{
		Templates: []Template{{Name: "default", Kernel: "/x", RootFS: "/y"}},
		Tokens:    map[string]string{"t": "s"},
	}, log)
	if err != nil {
		t.Fatal(err)
	}
	if s.audit == nil || s.audit.Enabled() {
		t.Fatalf("expected a disabled recorder, got %#v", s.audit)
	}
}

func TestNewUsesProvidedRecorder(t *testing.T) {
	log := logrus.New()
	log.SetLevel(logrus.ErrorLevel)
	rec, err := audit.New(audit.Config{Dir: t.TempDir(), Metrics: audit.NopMetrics(), Log: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rec.Close() })
	s, err := New(Config{
		Templates: []Template{{Name: "default", Kernel: "/x", RootFS: "/y"}},
		Tokens:    map[string]string{"t": "s"},
		Audit:     rec,
	}, log)
	if err != nil {
		t.Fatal(err)
	}
	if s.audit != rec {
		t.Fatal("server did not adopt the provided recorder")
	}
}

func TestAuditAll(t *testing.T) {
	if !(TokenPolicy{AuditRead: "all"}).auditAll() {
		t.Fatal(`"all" should enable full audit read`)
	}
	if (TokenPolicy{}).auditAll() {
		t.Fatal("empty AuditRead should not enable full audit read")
	}
}
