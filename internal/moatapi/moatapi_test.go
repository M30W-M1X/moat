package moatapi

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestAuditAndReport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/audit":
			if r.URL.Query().Get("sandbox") != "sb-1" {
				t.Errorf("missing sandbox filter: %s", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.Write([]byte(`{"seq":1,"ts":"2026-08-30T18:00:00Z","type":"sandbox.create","sandbox":"sb-1","token":"ci","attrs":{"template":"python"}}` + "\n"))
			w.Write([]byte(`{"seq":2,"ts":"2026-08-30T18:00:01Z","type":"exec","sandbox":"sb-1","token":"ci","attrs":{"exit":0}}` + "\n"))
		case "/v1/sandboxes/sb-1/report":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"sandbox":"sb-1","token":"ci","template":"python","counts":{"exec":1},"exec":{"n":1,"nonzero":0},"bytes":{"in":0,"out":0}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	evs, err := c.Audit(context.Background(), AuditFilter{Sandbox: "sb-1", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[1].Type != "exec" {
		t.Fatalf("audit events wrong: %+v", evs)
	}
	rep, err := c.Report(context.Background(), "sb-1")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Sandbox != "sb-1" || rep.Exec.N != 1 {
		t.Fatalf("report wrong: %+v", rep)
	}
}

func TestUnixSocketClient(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "moatd.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sandboxes":[{"id":"sb-unix","template":"default"}]}`))
		}),
	}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	for _, c := range []*Client{NewUnix(sock, "tok"), New("unix://"+sock, "tok")} {
		sbs, err := c.List(context.Background())
		if err != nil {
			t.Fatalf("List over unix socket: %v", err)
		}
		if len(sbs) != 1 || sbs[0].ID != "sb-unix" {
			t.Fatalf("unexpected sandboxes: %+v", sbs)
		}
	}
}
