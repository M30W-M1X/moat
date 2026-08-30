package secretscan

import (
	"strings"
	"testing"
)

func mustNew(t *testing.T) *Scanner {
	t.Helper()
	s, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.RuleCount() < 150 {
		t.Fatalf("only %d rules loaded (skipped %d) — TOML parse looks wrong", s.RuleCount(), s.Skipped())
	}
	return s
}

func hasRule(fs []Finding, id string) bool {
	for _, f := range fs {
		if f.Rule == id {
			return true
		}
	}
	return false
}

func TestScanDetectsKnownFormats(t *testing.T) {
	s := mustNew(t)

	// Realistic-shaped fakes (not real credentials).
	cases := []struct {
		name    string
		content string
		want    string // a rule id that must fire
	}{
		{"gh.txt", "GITHUB_TOKEN=ghp_" + "aB3xK9mQ2pR7tW1zC4vN6yH8jL0dF5gS2kD9", "github-pat"},
		{"slack.env", "SLACK_BOT_TOKEN=xoxb-" + "2846175927-3910284756-" + "Kd8Xm2Qp9Rt4Wz1Cv6Ny", "slack-bot-token"},
		{"key.pem", "-----BEGIN OPENSSH PRIVATE KEY-----\n" +
			"b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZWQyNTUx\n" +
			"OQAAACA0Kx9pQmZ7vN3wR8tY2uC5aB6dE1fG7hJ0kL9mP4qXw\n" +
			"-----END OPENSSH PRIVATE KEY-----\n", "private-key"},
		{"cfg.yaml", "service:\n  api_key: \"" + "9f2Kx7Qm3Rt8Wz1Cv6Ny4Bp0Hd5Jg2Lf8Sk9Xa\"\n", "generic-api-key"},
	}
	for _, c := range cases {
		fs := s.Scan(c.name, []byte(c.content))
		if !hasRule(fs, c.want) {
			t.Errorf("%s: want rule %q, got %v", c.name, c.want, fs)
		}
	}
}

func TestScanIgnoresCleanContent(t *testing.T) {
	s := mustNew(t)
	clean := "name = widget-service\n" +
		"version = 1.4.2\n" +
		"listen_port = 8443\n" +
		"log_level = debug\n" +
		"# a comment about the config\n"
	if fs := s.Scan("app.toml", []byte(clean)); len(fs) != 0 {
		t.Errorf("clean content produced findings: %v", fs)
	}
}

func TestScanEntropyFloor(t *testing.T) {
	s := mustNew(t)
	// Matches the ghp_ shape but the body is a low-entropy repeat, below the
	// rule's entropy floor of 3.
	low := "token=ghp_" + strings.Repeat("a1", 18)
	if fs := s.Scan("x", []byte(low)); hasRule(fs, "github-pat") {
		t.Errorf("low-entropy token should be below the floor: %v", fs)
	}
}

func TestScanReportsLineAndMask(t *testing.T) {
	s := mustNew(t)
	content := "line one\nline two\nAWS_SECRET_ACCESS_KEY=aZ2bYc4dXe6fWg8hVi0jUk2lTm4nSo6pRq8rQs0t\nline four\n"
	fs := s.Scan("creds", []byte(content))
	if len(fs) == 0 {
		t.Fatal("expected a finding")
	}
	f := fs[0]
	if f.Line != 3 {
		t.Errorf("line = %d, want 3", f.Line)
	}
	if strings.Contains(f.Secret, "aZ2bYc4dXe6fWg8h") {
		t.Errorf("Secret is not masked: %q", f.Secret)
	}
	if !strings.HasPrefix(f.Secret, "aZ") || !strings.Contains(f.Secret, "chars)") {
		t.Errorf("mask shape unexpected: %q", f.Secret)
	}
}

func TestRedactReplacesOnlyTheSecret(t *testing.T) {
	s := mustNew(t)
	content := []byte("before\nAWS_SECRET_ACCESS_KEY=aZ2bYc4dXe6fWg8hVi0jUk2lTm4nSo6pRq8rQs0t\nafter\n")
	fs := s.Scan("creds", content)
	if len(fs) == 0 {
		t.Fatal("expected a finding")
	}
	red := string(s.Redact(content, fs))
	if strings.Contains(red, "aZ2bYc4dXe6fWg8h") {
		t.Errorf("redacted output still holds the secret:\n%s", red)
	}
	if !strings.HasPrefix(red, "before\n") || !strings.HasSuffix(red, "\nafter\n") {
		t.Errorf("redaction damaged surrounding bytes:\n%s", red)
	}
	if !strings.Contains(red, "***REDACTED:") {
		t.Errorf("no redaction marker:\n%s", red)
	}
	// The input slice must be untouched.
	if strings.Contains(string(content), "***REDACTED") {
		t.Error("Redact mutated its input")
	}
}

func TestShannon(t *testing.T) {
	if e := shannon("aaaaaaaa"); e != 0 {
		t.Errorf("all-same-char entropy = %v, want 0", e)
	}
	if e := shannon("ab"); e < 0.99 || e > 1.01 {
		t.Errorf("two-symbol entropy = %v, want ~1", e)
	}
	if shannon("Kd8Xm2Qp9Rt4Wz1Cv6Ny") < 3.5 {
		t.Error("a mixed 20-char token should be well above 3.5 bits")
	}
}
