//go:build integration

package server_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/M30W-M1X/moat/internal/config"
	"github.com/M30W-M1X/moat/internal/server"
)

func startServer(t *testing.T, poolSize, tokenMax int) (*httptest.Server, string) {
	t.Helper()
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("no /dev/kvm")
	}
	m, err := config.NewMachine("srvtest-tpl")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(); err != nil {
		t.Skipf("images missing (run: make images): %v", err)
	}

	log := logrus.New()
	log.SetLevel(logrus.WarnLevel)
	srv, err := server.New(server.Config{
		Templates: []server.Template{{
			Name: "default", Kernel: m.KernelImagePath, RootFS: m.RootFSPath, VCPUs: 1, MemMiB: 256,
		}},
		Tokens:   map[string]string{"t1": "secret1", "t2": "secret2"},
		PoolSize: poolSize,
		TokenMax: tokenMax,
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
		sc, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		srv.Shutdown(sc)
	})
	return ts, "secret1"
}

func do(t *testing.T, ts *httptest.Server, method, path, token, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func TestAPILifecycle(t *testing.T) {
	ts, token := startServer(t, 0, 4)

	// Auth is required.
	noAuth := do(t, ts, "GET", "/v1/sandboxes", "", "")
	noAuth.Body.Close()
	if noAuth.StatusCode != 401 {
		t.Fatalf("no-token list = %d, want 401", noAuth.StatusCode)
	}

	// Create.
	resp := do(t, ts, "POST", "/v1/sandboxes", token, `{"ttl_seconds":120}`)
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create = %d: %s", resp.StatusCode, b)
	}
	var sb struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&sb)
	resp.Body.Close()
	if sb.ID == "" {
		t.Fatal("create returned no id")
	}
	t.Cleanup(func() { do(t, ts, "DELETE", "/v1/sandboxes/"+sb.ID, token, "").Body.Close() })

	// Exec: stream NDJSON, collect stdout and the exit frame.
	execBody := `{"argv":["sh","-c","echo out; echo err 1>&2; exit 5"]}`
	er := do(t, ts, "POST", "/v1/sandboxes/"+sb.ID+"/exec", token, execBody)
	if er.StatusCode != 200 {
		b, _ := io.ReadAll(er.Body)
		t.Fatalf("exec = %d: %s", er.StatusCode, b)
	}
	var gotOut, gotErr string
	var exitCode = -999
	sc := bufio.NewScanner(er.Body)
	for sc.Scan() {
		var ev map[string]any
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("bad NDJSON frame %q: %v", sc.Text(), err)
		}
		switch ev["stream"] {
		case "stdout":
			d, _ := base64.StdEncoding.DecodeString(ev["data"].(string))
			gotOut += string(d)
		case "stderr":
			d, _ := base64.StdEncoding.DecodeString(ev["data"].(string))
			gotErr += string(d)
		case "exit":
			exitCode = int(ev["code"].(float64))
		}
	}
	er.Body.Close()
	if gotOut != "out\n" || gotErr != "err\n" || exitCode != 5 {
		t.Fatalf("exec streams: out=%q err=%q code=%d", gotOut, gotErr, exitCode)
	}

	// File round trip.
	payload := "phase-5 file contents\n"
	pr := do(t, ts, "PUT", "/v1/sandboxes/"+sb.ID+"/files/tmp/p5.txt", token, payload)
	if pr.StatusCode != 200 {
		t.Fatalf("put file = %d", pr.StatusCode)
	}
	pr.Body.Close()
	gr := do(t, ts, "GET", "/v1/sandboxes/"+sb.ID+"/files/tmp/p5.txt", token, "")
	got, _ := io.ReadAll(gr.Body)
	gr.Body.Close()
	if string(got) != payload {
		t.Fatalf("get file = %q, want %q", got, payload)
	}

	// Another token cannot see this sandbox.
	other := do(t, ts, "GET", "/v1/sandboxes/"+sb.ID, "secret2", "")
	if other.StatusCode != 404 {
		t.Fatalf("cross-token GET = %d, want 404", other.StatusCode)
	}
	other.Body.Close()

	// Metrics are exported.
	mr := do(t, ts, "GET", "/metrics", "", "")
	mb, _ := io.ReadAll(mr.Body)
	mr.Body.Close()
	if !bytes.Contains(mb, []byte("moat_sandboxes_created_total")) {
		t.Error("/metrics missing moat_sandboxes_created_total")
	}
}

func TestQuota(t *testing.T) {
	ts, token := startServer(t, 0, 1)
	r1 := do(t, ts, "POST", "/v1/sandboxes", token, "")
	if r1.StatusCode != 201 {
		t.Fatalf("first create = %d", r1.StatusCode)
	}
	var sb struct {
		ID string `json:"id"`
	}
	json.NewDecoder(r1.Body).Decode(&sb)
	r1.Body.Close()

	r2 := do(t, ts, "POST", "/v1/sandboxes", token, "")
	if r2.StatusCode != 429 {
		t.Fatalf("second create = %d, want 429", r2.StatusCode)
	}
	r2.Body.Close()

	do(t, ts, "DELETE", "/v1/sandboxes/"+sb.ID, token, "").Body.Close()
	// Give the async destroy a moment, then a new create should succeed.
	time.Sleep(6 * time.Second)
	r3 := do(t, ts, "POST", "/v1/sandboxes", token, "")
	if r3.StatusCode != 201 {
		t.Fatalf("create after release = %d, want 201", r3.StatusCode)
	}
	json.NewDecoder(r3.Body).Decode(&sb)
	r3.Body.Close()
	do(t, ts, "DELETE", "/v1/sandboxes/"+sb.ID, token, "").Body.Close()
}
