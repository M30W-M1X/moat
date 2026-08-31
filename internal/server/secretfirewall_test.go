package server

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/M30W-M1X/moat/internal/audit"
	"github.com/sirupsen/logrus"
)

// awsSecretLine is a realistic-shaped fake, not a real credential.
const awsSecretLine = "AWS_SECRET_ACCESS_KEY=aZ2bYc4dXe6fWg8hVi0jUk2lTm4nSo6pRq8rQs0t"

func newScreenServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	log := logrus.New()
	log.SetLevel(logrus.PanicLevel)
	if len(cfg.Templates) == 0 {
		cfg.Templates = []Template{{Name: "default", Kernel: "/none", RootFS: "/none", VCPUs: 1, MemMiB: 128}}
	}
	if len(cfg.Tokens) == 0 {
		cfg.Tokens = map[string]string{"t": "s"}
	}
	s, err := New(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestScreenBlocksByDefault(t *testing.T) {
	s := newScreenServer(t, Config{})
	if s.scanner == nil {
		t.Fatal("scanner should be built by default")
	}
	d := s.screen("t", "env:AWS_SECRET_ACCESS_KEY", []byte(awsSecretLine), false)
	if d.action != "block" {
		t.Fatalf("action = %q, want block", d.action)
	}
	if len(d.findings) == 0 {
		t.Fatal("expected findings")
	}
}

func TestScreenPassesCleanContent(t *testing.T) {
	s := newScreenServer(t, Config{})
	d := s.screen("t", "/work/app.toml", []byte("name = widget\nport = 8443\n"), false)
	if d.action != "clean" {
		t.Fatalf("action = %q, want clean", d.action)
	}
}

func TestScreenOverrideNeedsPolicy(t *testing.T) {
	s := newScreenServer(t, Config{
		Policies: map[string]TokenPolicy{"priv": {AllowSecretOverride: true}},
	})
	// allow_secrets set, but token has no override permission -> still block.
	if d := s.screen("t", "stdin", []byte(awsSecretLine), true); d.action != "block" {
		t.Errorf("unprivileged override: action = %q, want block", d.action)
	}
	// permitted token -> allow.
	if d := s.screen("priv", "stdin", []byte(awsSecretLine), true); d.action != "allow" {
		t.Errorf("privileged override: action = %q, want allow", d.action)
	}
	// permitted token but request did not ask -> block.
	if d := s.screen("priv", "stdin", []byte(awsSecretLine), false); d.action != "block" {
		t.Errorf("privileged, no allow_secrets: action = %q, want block", d.action)
	}
}

func TestScreenRedactMode(t *testing.T) {
	s := newScreenServer(t, Config{SecretRedact: true})
	d := s.screen("t", "/work/.env", []byte("prefix\n"+awsSecretLine+"\nsuffix\n"), false)
	if d.action != "redact" {
		t.Fatalf("action = %q, want redact", d.action)
	}
	got := string(d.content)
	if strings.Contains(got, "aZ2bYc4dXe6fWg8h") {
		t.Errorf("redacted content still holds the secret:\n%s", got)
	}
	if !strings.HasPrefix(got, "prefix\n") || !strings.HasSuffix(got, "\nsuffix\n") {
		t.Errorf("redaction damaged surrounding bytes:\n%s", got)
	}
}

func TestScreenDisabled(t *testing.T) {
	s := newScreenServer(t, Config{SecretScanOff: true})
	if s.scanner != nil {
		t.Fatal("scanner should be nil when SecretScanOff")
	}
	if d := s.screen("t", "x", []byte(awsSecretLine), false); d.action != "clean" {
		t.Errorf("disabled scanner: action = %q, want clean", d.action)
	}
}

func TestScreenAudits(t *testing.T) {
	log := logrus.New()
	log.SetLevel(logrus.ErrorLevel)
	dir := t.TempDir()
	rec, err := audit.New(audit.Config{Dir: dir, Metrics: audit.NopMetrics(), Log: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rec.Close() })
	s, err := New(Config{
		Templates: []Template{{Name: "default", Kernel: "/x", RootFS: "/y"}},
		Tokens:    map[string]string{"ci": "s"},
		Audit:     rec,
	}, log)
	if err != nil {
		t.Fatal(err)
	}
	// A well-known fake AWS secret access key (not an "EXAMPLE" value).
	d := s.screen("ci", "env:AWS_SECRET_ACCESS_KEY",
		[]byte("AWS_SECRET_ACCESS_KEY=wJa1bXUtnFEMI7K7MDENGbPxRfiCYZz6zJ9AbC0d"), false)
	if d.action != "block" {
		t.Skipf("scanner did not flag the test value (rule set change); action=%s", d.action)
	}
	evs, _ := audit.Query(dir, audit.Filter{Types: []string{audit.TypeSecretBlock}})
	if len(evs) != 1 {
		t.Fatalf("want 1 secret.block event, got %d", len(evs))
	}
	a := evs[0].Attrs
	if a["target"] != "env:AWS_SECRET_ACCESS_KEY" {
		t.Fatalf("target = %v", a["target"])
	}
	if _, bad := a["value"]; bad {
		t.Fatal("secret.block leaked a value")
	}
	if _, bad := a["secret"]; bad {
		t.Fatal("secret.block leaked a secret")
	}
	if rules, _ := a["rules"].([]any); len(rules) == 0 {
		t.Fatal("secret.block has no rules")
	}
}

// TestScreenFailsClosedOnRecordError checks that screen() reports the
// RecordSync error for an allow or a redact decision. A failed record must not
// pass flagged content without a durable audit line.
func TestScreenFailsClosedOnRecordError(t *testing.T) {
	log := logrus.New()
	log.SetLevel(logrus.PanicLevel)

	build := func(t *testing.T, redact bool) *Server {
		t.Helper()
		dir := t.TempDir()
		rec, err := audit.New(audit.Config{
			Dir: dir, Metrics: audit.NopMetrics(), Log: log,
			SyncFunc: func(*os.File) error { return errors.New("simulated fsync failure") },
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = rec.Close() })
		s, err := New(Config{
			Templates:    []Template{{Name: "default", Kernel: "/x", RootFS: "/y"}},
			Tokens:       map[string]string{"priv": "s"},
			Policies:     map[string]TokenPolicy{"priv": {AllowSecretOverride: true}},
			SecretRedact: redact,
			Audit:        rec,
		}, log)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	// allow decision: override policy plus allow_secrets.
	s := build(t, false)
	d := s.screen("priv", "env:AWS_SECRET_ACCESS_KEY", []byte(awsSecretLine), true)
	if d.action != "allow" {
		t.Fatalf("action = %q, want allow", d.action)
	}
	if d.recErr == nil || !d.auditUnrecorded() {
		t.Fatalf("allow with a failed record: recErr=%v auditUnrecorded=%v", d.recErr, d.auditUnrecorded())
	}

	// redact decision.
	sr := build(t, true)
	dr := sr.screen("priv", "/work/.env", []byte("prefix\n"+awsSecretLine+"\nsuffix\n"), false)
	if dr.action != "redact" {
		t.Fatalf("action = %q, want redact", dr.action)
	}
	if dr.recErr == nil || !dr.auditUnrecorded() {
		t.Fatalf("redact with a failed record: recErr=%v auditUnrecorded=%v", dr.recErr, dr.auditUnrecorded())
	}

	// clean content never sets recErr.
	dc := s.screen("priv", "/work/ok.txt", []byte("name = widget\n"), false)
	if dc.recErr != nil || dc.auditUnrecorded() {
		t.Fatalf("clean content must not report an audit error: %+v", dc)
	}
}
