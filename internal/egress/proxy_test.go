package egress

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

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
