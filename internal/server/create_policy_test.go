package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

// newPolicyServer builds a server with two templates and a restricted token.
// It never boots a VM: every assertion here checks a rejection that happens
// before coldBoot.
func newPolicyServer(t *testing.T) *httptest.Server {
	t.Helper()
	log := logrus.New()
	log.SetLevel(logrus.PanicLevel)
	srv, err := New(Config{
		Templates: []Template{
			{Name: "default", Kernel: "/none", RootFS: "/none", VCPUs: 1, MemMiB: 128},
			{Name: "gpu", Kernel: "/none", RootFS: "/none", VCPUs: 1, MemMiB: 128},
		},
		Tokens: map[string]string{"ci": "s3cr3t", "admin": "root"},
		Policies: map[string]TokenPolicy{
			"ci": {Templates: []string{"default"}, MaxTTL: time.Minute, DenyNetwork: true},
		},
	}, log)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sc)
	})
	return ts
}

func post(t *testing.T, ts *httptest.Server, token, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/sandboxes", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestPolicyBlocksDisallowedTemplate(t *testing.T) {
	ts := newPolicyServer(t)
	if code, body := post(t, ts, "s3cr3t", `{"template":"gpu"}`); code != http.StatusForbidden {
		t.Fatalf("ci -> gpu: code %d body %s, want 403", code, body)
	}
}

func TestPolicyBlocksNetworkedSandbox(t *testing.T) {
	ts := newPolicyServer(t)
	if code, body := post(t, ts, "s3cr3t", `{"template":"default","networked":true}`); code != http.StatusForbidden {
		t.Fatalf("ci networked: code %d body %s, want 403", code, body)
	}
}

func TestPolicyDoesNotBlockUnrestrictedToken(t *testing.T) {
	ts := newPolicyServer(t)
	// admin has no policy. The gpu template is allowed, so the request gets
	// past the policy checks and fails later at boot (500), not at 403.
	code, body := post(t, ts, "root", `{"template":"gpu"}`)
	if code == http.StatusForbidden {
		t.Fatalf("admin -> gpu was forbidden, should be allowed: %s", body)
	}
}
