package egress

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"testing"
	"time"

	"github.com/M30W-M1X/moat/internal/audit"
	"github.com/sirupsen/logrus"
)

func TestPolicyPermits(t *testing.T) {
	p := Policy{Allow: []string{"example.com", "*.pypi.org"}, Deny: []string{"evil.example.com"}}
	cases := map[string]bool{
		"example.com":      true,
		"api.example.com":  true,
		"files.pypi.org":   true,
		"pypi.org":         true,
		"evil.example.com": false, // deny wins over allow
		"github.com":       false, // not in allow
		"notexample.com":   false, // suffix must be on a dot boundary
	}
	for host, want := range cases {
		if got := p.Permits(host); got != want {
			t.Errorf("Permits(%q) = %v, want %v", host, got, want)
		}
	}

	open := Policy{}
	if !open.Permits("anything.test") {
		t.Error("empty policy should allow everything")
	}
	if (Policy{Deny: []string{"blocked.test"}}).Permits("blocked.test") {
		t.Error("deny should block even with empty allow")
	}
}

func TestDecideRecordsAudit(t *testing.T) {
	dir := t.TempDir()
	log := logrus.New()
	log.SetLevel(logrus.ErrorLevel)
	rec, err := audit.New(audit.Config{Dir: dir, Metrics: audit.NopMetrics(), Log: log,
		FsyncBatch: 1, FsyncInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rec.Close() })

	p := New(Policy{Allow: []string{"pypi.org"}}, log, nil)
	p.SetAudit(rec, func(ip netip.Addr) (string, string, bool) {
		if ip == netip.MustParseAddr("10.0.0.2") {
			return "sb-42", "ci", true
		}
		return "", "", false
	})

	req := httptest.NewRequest(http.MethodGet, "http://pypi.org/simple/", nil)
	req.RemoteAddr = "10.0.0.2:5555"
	if !p.decide(req, "pypi.org", "proxy") {
		t.Fatal("pypi.org should be allowed")
	}
	req2 := httptest.NewRequest(http.MethodGet, "http://evil.example/", nil)
	req2.RemoteAddr = "10.0.0.2:5556"
	if p.decide(req2, "evil.example", "proxy") {
		t.Fatal("evil.example should be denied")
	}
	time.Sleep(100 * time.Millisecond)

	allow, _ := audit.Query(dir, audit.Filter{Types: []string{audit.TypeEgressAllow}})
	deny, _ := audit.Query(dir, audit.Filter{Types: []string{audit.TypeEgressDeny}})
	if len(allow) != 1 || allow[0].Sandbox != "sb-42" || allow[0].Attrs["host"] != "pypi.org" {
		t.Fatalf("allow event wrong: %+v", allow)
	}
	if allow[0].Attrs["matched_rule"] != "pypi.org" {
		t.Fatalf("allow matched_rule = %v, want pypi.org", allow[0].Attrs["matched_rule"])
	}
	if len(deny) != 1 || deny[0].Attrs["host"] != "evil.example" {
		t.Fatalf("deny event wrong: %+v", deny)
	}
	if deny[0].Attrs["matched_rule"] != "default-deny" {
		t.Fatalf("deny matched_rule = %v, want default-deny", deny[0].Attrs["matched_rule"])
	}
}

func TestPolicyMatch(t *testing.T) {
	cases := []struct {
		pol       Policy
		host      string
		wantRule  string
		wantAllow bool
	}{
		{Policy{}, "anything.test", "default-allow", true},
		{Policy{Deny: []string{"blocked.test"}}, "api.blocked.test", "blocked.test", false},
		{Policy{Allow: []string{"pypi.org"}}, "pypi.org", "pypi.org", true},
		{Policy{Allow: []string{"pypi.org"}}, "evil.example", "default-deny", false},
		{Policy{Allow: []string{"a.test"}, Deny: []string{"a.test"}}, "a.test", "a.test", false},
	}
	for _, c := range cases {
		rule, allow := c.pol.Match(c.host)
		if rule != c.wantRule || allow != c.wantAllow {
			t.Errorf("Match(%q) = (%q, %v), want (%q, %v)", c.host, rule, allow, c.wantRule, c.wantAllow)
		}
	}
}

func TestProxyAllowsAndDenies(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "hello from upstream")
	}))
	defer upstream.Close()
	upURL, _ := url.Parse(upstream.URL)

	log := logrus.New()
	log.SetLevel(logrus.PanicLevel)
	px := New(Policy{Allow: []string{upURL.Hostname()}}, log, nil)
	proxySrv := httptest.NewServer(px)
	defer proxySrv.Close()
	proxyURL, _ := url.Parse(proxySrv.URL)

	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	// Allowed host (upstream is 127.0.0.1, which we put in Allow).
	resp, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatalf("allowed GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "hello from upstream" {
		t.Fatalf("allowed GET = %d %q", resp.StatusCode, body)
	}

	// Denied host: same proxy, different Host. Build the request by hand so the
	// proxy sees an absolute URL to a host not in the allow list.
	req, _ := http.NewRequest("GET", "http://blocked.invalid/", nil)
	resp2, err := client.Do(req)
	if err != nil {
		t.Fatalf("denied GET: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Errorf("denied GET status = %d, want 403", resp2.StatusCode)
	}
}

func TestPerTokenEgressPolicy(t *testing.T) {
	log := logrus.New()
	log.SetLevel(logrus.ErrorLevel)

	p := New(Policy{
		Allow: []string{"pypi.org", "github.com"},
		Deny:  []string{"blocked.global.test"},
	}, log, nil)
	p.SetAudit(nil, func(ip netip.Addr) (string, string, bool) {
		switch ip.String() {
		case "10.0.0.2":
			return "sb-ci", "ci", true
		case "10.0.0.3":
			return "sb-dev", "dev", true
		default:
			return "", "", false
		}
	})
	p.SetPolicyResolver(func(tid string) (Policy, bool) {
		switch tid {
		case "ci":
			return Policy{
				Allow: []string{"registry.npmjs.org", "blocked.global.test"},
				Deny:  []string{"bad.registry.npmjs.org"},
			}, true
		case "dev":
			return Policy{Deny: []string{"github.com"}}, true
		default:
			return Policy{}, false
		}
	})

	check := func(remote, host string, want bool) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
		req.RemoteAddr = remote + ":12345"
		if got := p.decide(req, host, "proxy"); got != want {
			t.Errorf("remote=%s host=%s: got %v, want %v", remote, host, got, want)
		}
	}

	// Token "ci" has explicit allow list and deny list.
	check("10.0.0.2", "registry.npmjs.org", true)
	check("10.0.0.2", "bad.registry.npmjs.org", false) // token deny wins
	check("10.0.0.2", "pypi.org", false)               // not in token allow
	check("10.0.0.2", "blocked.global.test", false)    // global deny always wins over token allow

	// Token "dev" specifies only a deny override; global allow still applies.
	check("10.0.0.3", "pypi.org", true)
	check("10.0.0.3", "github.com", false) // token deny blocks global allow
	check("10.0.0.3", "evil.example", false)

	// Unknown client IP falls back to global policy.
	check("10.0.0.9", "github.com", true)
	check("10.0.0.9", "registry.npmjs.org", false)
}
