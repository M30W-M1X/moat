//go:build integration

package server_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/M30W-M1X/moat/internal/audit"
	"github.com/M30W-M1X/moat/internal/config"
	"github.com/M30W-M1X/moat/internal/server"
)

// copyRootFS returns a per-test sparse copy of src. A cold-boot sandbox
// attaches the rootfs read-write, so a shared build/artifacts/rootfs.ext4 would
// drift between tests and trip the images.lock check. A private copy keeps each
// test isolated and leaves no images.lock beside it.
func copyRootFS(t *testing.T, src string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "rootfs.ext4")
	out, err := exec.Command("cp", "--sparse=always", src, dst).CombinedOutput()
	if err != nil {
		t.Fatalf("copy rootfs: %v: %s", err, out)
	}
	return dst
}

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
			Name: "default", Kernel: m.KernelImagePath, RootFS: copyRootFS(t, m.RootFSPath), VCPUs: 1, MemMiB: 256,
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

func startServerWithAudit(t *testing.T, auditDir string) (*httptest.Server, string) {
	t.Helper()
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("no /dev/kvm")
	}
	m, err := config.NewMachine("srvtest-tpl")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(); err != nil {
		t.Skipf("images missing: %v", err)
	}
	log := logrus.New()
	log.SetLevel(logrus.WarnLevel)
	rec, err := audit.New(audit.Config{Dir: auditDir, Metrics: audit.NopMetrics(), Log: log,
		FsyncBatch: 4, FsyncInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(server.Config{
		Templates: []server.Template{{Name: "default", Kernel: m.KernelImagePath, RootFS: copyRootFS(t, m.RootFSPath), VCPUs: 1, MemMiB: 256}},
		Tokens:    map[string]string{"t1": "secret1", "secops": "s3", "stranger": "s4"},
		Policies:  map[string]server.TokenPolicy{"secops": {AuditRead: "all"}},
		TokenMax:  4,
		Audit:     rec,
		Version:   "test",
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
		_ = rec.Close()
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

func TestAuditLifecycle(t *testing.T) {
	dir := t.TempDir()
	ts, token := startServerWithAudit(t, dir)

	resp := do(t, ts, "POST", "/v1/sandboxes", token, `{"ttl_seconds":120}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create = %d", resp.StatusCode)
	}
	var sb struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&sb)
	resp.Body.Close()

	del := do(t, ts, "DELETE", "/v1/sandboxes/"+sb.ID, token, "")
	del.Body.Close()
	if del.StatusCode != 204 {
		t.Fatalf("delete = %d", del.StatusCode)
	}

	// Give the async writer a beat, then read the files directly.
	time.Sleep(200 * time.Millisecond)
	evs, err := audit.Query(dir, audit.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range evs {
		seen[e.Type] = true
		if e.Type == audit.TypeSandboxCreate && e.Sandbox != sb.ID {
			t.Fatalf("create event has sandbox %q, want %q", e.Sandbox, sb.ID)
		}
	}
	for _, want := range []string{audit.TypeDaemonStart, audit.TypeSandboxCreate, audit.TypeSandboxDestroy} {
		if !seen[want] {
			t.Fatalf("missing %s in audit log; saw %v", want, seen)
		}
	}
	// The destroy event carries a reason.
	for _, e := range evs {
		if e.Type == audit.TypeSandboxDestroy {
			if r, _ := e.Attrs["reason"].(string); r != "deleted" {
				t.Fatalf("destroy reason = %q, want deleted", r)
			}
		}
	}
}

func TestAuditExecAndFiles(t *testing.T) {
	dir := t.TempDir()
	ts, token := startServerWithAudit(t, dir)

	resp := do(t, ts, "POST", "/v1/sandboxes", token, `{"ttl_seconds":120}`)
	var sb struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&sb)
	resp.Body.Close()

	put := do(t, ts, "PUT", "/v1/sandboxes/"+sb.ID+"/files/work/hello.txt", token, "hi there")
	put.Body.Close()
	if put.StatusCode != 200 {
		t.Fatalf("put = %d", put.StatusCode)
	}
	ex := do(t, ts, "POST", "/v1/sandboxes/"+sb.ID+"/exec", token, `{"argv":["/bin/echo","ok"],"timeout_ms":5000}`)
	io.Copy(io.Discard, ex.Body)
	ex.Body.Close()
	get := do(t, ts, "GET", "/v1/sandboxes/"+sb.ID+"/files/work/hello.txt", token, "")
	io.Copy(io.Discard, get.Body)
	get.Body.Close()

	time.Sleep(300 * time.Millisecond)
	evs, _ := audit.Query(dir, audit.Filter{Sandbox: sb.ID})
	kinds := map[string]audit.Event{}
	for _, e := range evs {
		kinds[e.Type] = e
	}
	for _, want := range []string{audit.TypeFilePut, audit.TypeExec, audit.TypeFileGet} {
		if _, ok := kinds[want]; !ok {
			t.Fatalf("missing %s; saw %v", want, kinds)
		}
	}
	// exec attrs: argv present, no output.
	ea := kinds[audit.TypeExec].Attrs
	if _, ok := ea["argv"]; !ok {
		t.Fatal("exec event has no argv")
	}
	for _, forbidden := range []string{"stdout", "stderr", "output", "content", "value", "stdin"} {
		if _, bad := ea[forbidden]; bad {
			t.Fatalf("exec event leaked %q", forbidden)
		}
	}
	if exit, _ := ea["exit"].(float64); exit != 0 {
		t.Fatalf("exec exit = %v, want 0", ea["exit"])
	}
	// file.put attrs: path + bytes + sha256 + scan, no content.
	pa := kinds[audit.TypeFilePut].Attrs
	if pa["path"] != "/work/hello.txt" || int(pa["bytes"].(float64)) != 8 {
		t.Fatalf("file.put attrs wrong: %v", pa)
	}
	if _, bad := pa["content"]; bad {
		t.Fatal("file.put leaked content")
	}
}

func TestAuditReport(t *testing.T) {
	dir := t.TempDir()
	ts, ciToken := startServerWithAudit(t, dir)

	resp := do(t, ts, "POST", "/v1/sandboxes", ciToken, `{"ttl_seconds":120}`)
	var sb struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&sb)
	resp.Body.Close()

	put := do(t, ts, "PUT", "/v1/sandboxes/"+sb.ID+"/files/work/a.txt", ciToken, "hello")
	put.Body.Close()
	ex := do(t, ts, "POST", "/v1/sandboxes/"+sb.ID+"/exec", ciToken, `{"argv":["/bin/false"],"timeout_ms":3000}`)
	io.Copy(io.Discard, ex.Body)
	ex.Body.Close()
	del := do(t, ts, "DELETE", "/v1/sandboxes/"+sb.ID, ciToken, "")
	del.Body.Close()
	time.Sleep(300 * time.Millisecond)

	// The report works after destroy for the owner.
	rep := do(t, ts, "GET", "/v1/sandboxes/"+sb.ID+"/report", ciToken, "")
	defer rep.Body.Close()
	if rep.StatusCode != 200 {
		t.Fatalf("report = %d", rep.StatusCode)
	}
	var r struct {
		Sandbox string `json:"sandbox"`
		Exec    struct {
			N       int `json:"n"`
			Nonzero int `json:"nonzero"`
		} `json:"exec"`
		Bytes struct {
			In int64 `json:"in"`
		} `json:"bytes"`
		DestroyReason string `json:"destroy_reason"`
	}
	json.NewDecoder(rep.Body).Decode(&r)
	// "hello" is 5 bytes. bytes.in arrives as a JSON number.
	if r.Sandbox != sb.ID || r.Exec.N != 1 || r.Exec.Nonzero != 1 || r.Bytes.In != int64(len("hello")) || r.DestroyReason != "deleted" {
		t.Fatalf("report body wrong: %+v", r)
	}

	// A bad token fails auth first.
	bad := do(t, ts, "GET", "/v1/sandboxes/"+sb.ID+"/report", "wrongsecret", "")
	bad.Body.Close()
	if bad.StatusCode != 401 {
		t.Fatalf("bad-token report = %d", bad.StatusCode)
	}

	// A valid non-owner without "all" gets 404.
	stranger := do(t, ts, "GET", "/v1/sandboxes/"+sb.ID+"/report", "s4", "")
	stranger.Body.Close()
	if stranger.StatusCode != 404 {
		t.Fatalf("stranger report = %d, want 404", stranger.StatusCode)
	}
}

func TestAuditReadScoping(t *testing.T) {
	dir := t.TempDir()
	ts, ciToken := startServerWithAudit(t, dir) // "secret1" for t1/ci

	// t1 creates a sandbox and runs one exec.
	resp := do(t, ts, "POST", "/v1/sandboxes", ciToken, `{"ttl_seconds":120}`)
	var sb struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&sb)
	resp.Body.Close()
	ex := do(t, ts, "POST", "/v1/sandboxes/"+sb.ID+"/exec", ciToken, `{"argv":["/bin/true"],"timeout_ms":3000}`)
	io.Copy(io.Discard, ex.Body)
	ex.Body.Close()
	time.Sleep(300 * time.Millisecond)

	// A different token with no policy sees none of t1's events.
	other := do(t, ts, "GET", "/v1/audit", "s3wrong", "") // invalid token
	if other.StatusCode != 401 {
		t.Fatalf("bad token audit read = %d", other.StatusCode)
	}
	other.Body.Close()

	// secops (audit_read: all) sees t1's create + exec.
	sec := do(t, ts, "GET", "/v1/audit?sandbox="+sb.ID, "s3", "")
	defer sec.Body.Close()
	if sec.StatusCode != 200 {
		t.Fatalf("secops audit read = %d", sec.StatusCode)
	}
	n := 0
	dec := json.NewDecoder(sec.Body)
	for dec.More() {
		var e map[string]any
		if err := dec.Decode(&e); err != nil {
			break
		}
		n++
	}
	if n < 2 {
		t.Fatalf("secops saw %d events for %s, want >= 2", n, sb.ID)
	}

	// ci sees its own sandbox's events. The non-all path scopes each event
	// through the O(1) sandbox owner index, not a per-event log scan.
	own := do(t, ts, "GET", "/v1/audit?sandbox="+sb.ID, ciToken, "")
	defer own.Body.Close()
	m := 0
	sawCreate := false
	d2 := json.NewDecoder(own.Body)
	for d2.More() {
		var e map[string]any
		if d2.Decode(&e) != nil {
			break
		}
		if e["type"] == "sandbox.create" {
			sawCreate = true
		}
		m++
	}
	if m < 2 {
		t.Fatalf("ci saw %d of its own events, want >= 2", m)
	}
	if !sawCreate {
		t.Fatal("ci did not see the sandbox.create event for its own sandbox")
	}

	// A valid non-all token that owns nothing sees none of ci's events —
	// this locks the denial path, not just the auth gate.
	strangerScoped := do(t, ts, "GET", "/v1/audit?sandbox="+sb.ID, "s4", "")
	defer strangerScoped.Body.Close()
	if strangerScoped.StatusCode != 200 {
		t.Fatalf("stranger scoped audit read = %d, want 200", strangerScoped.StatusCode)
	}
	sc := 0
	d3 := json.NewDecoder(strangerScoped.Body)
	for d3.More() {
		var e map[string]any
		if d3.Decode(&e) != nil {
			break
		}
		sc++
	}
	if sc != 0 {
		t.Fatalf("stranger saw %d of ci's audit events, want 0 (scoping bypass)", sc)
	}

	strangerAll := do(t, ts, "GET", "/v1/audit", "s4", "")
	defer strangerAll.Body.Close()
	sa := 0
	d4 := json.NewDecoder(strangerAll.Body)
	for d4.More() {
		var e map[string]any
		if d4.Decode(&e) != nil {
			break
		}
		sa++
	}
	if sa != 0 {
		t.Fatalf("stranger unfiltered audit read saw %d events, want 0", sa)
	}
}

// TestAuditReadFilters exercises the type filter and the relative time window.
func TestAuditReadFilters(t *testing.T) {
	dir := t.TempDir()
	ts, ciToken := startServerWithAudit(t, dir)

	resp := do(t, ts, "POST", "/v1/sandboxes", ciToken, `{"ttl_seconds":120}`)
	var sb struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&sb)
	resp.Body.Close()
	ex := do(t, ts, "POST", "/v1/sandboxes/"+sb.ID+"/exec", ciToken, `{"argv":["/bin/true"],"timeout_ms":3000}`)
	io.Copy(io.Discard, ex.Body)
	ex.Body.Close()
	time.Sleep(300 * time.Millisecond)

	// A ?type=exec filter returns only exec events.
	byType := do(t, ts, "GET", "/v1/audit?sandbox="+sb.ID+"&type=exec", "s3", "")
	defer byType.Body.Close()
	if byType.StatusCode != 200 {
		t.Fatalf("type filter read = %d", byType.StatusCode)
	}
	execN := 0
	dt := json.NewDecoder(byType.Body)
	for dt.More() {
		var e map[string]any
		if dt.Decode(&e) != nil {
			break
		}
		if e["type"] != "exec" {
			t.Fatalf("type filter returned a %v event", e["type"])
		}
		execN++
	}
	if execN < 1 {
		t.Fatalf("type=exec returned %d events, want >= 1", execN)
	}

	// A ?since=-1h window still returns the recent events.
	win := do(t, ts, "GET", "/v1/audit?sandbox="+sb.ID+"&since=-1h", "s3", "")
	defer win.Body.Close()
	if win.StatusCode != 200 {
		t.Fatalf("since window read = %d", win.StatusCode)
	}
	winN := 0
	dw := json.NewDecoder(win.Body)
	for dw.More() {
		var e map[string]any
		if dw.Decode(&e) != nil {
			break
		}
		winN++
	}
	if winN < 2 {
		t.Fatalf("since=-1h returned %d events, want >= 2", winN)
	}

	// A malformed time window is a 400, not a silent full-log read.
	bad := do(t, ts, "GET", "/v1/audit?since=notatime", "s3", "")
	bad.Body.Close()
	if bad.StatusCode != 400 {
		t.Fatalf("since=notatime = %d, want 400", bad.StatusCode)
	}
}

func TestSecretFirewall(t *testing.T) {
	ts, token := startServer(t, 0, 4) // secret scanning is on by default

	resp := do(t, ts, "POST", "/v1/sandboxes", token, `{"ttl_seconds":120}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create = %d", resp.StatusCode)
	}
	var sb struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&sb)
	resp.Body.Close()
	t.Cleanup(func() { do(t, ts, "DELETE", "/v1/sandboxes/"+sb.ID, token, "").Body.Close() })

	// A realistic-shaped fake AWS secret, not a real credential.
	secretFile := "AWS_SECRET_ACCESS_KEY=aZ2bYc4dXe6fWg8hVi0jUk2lTm4nSo6pRq8rQs0t\n"

	// PUT a file that holds a secret -> 409.
	pr := do(t, ts, "PUT", "/v1/sandboxes/"+sb.ID+"/files/work/.env", token, secretFile)
	body, _ := io.ReadAll(pr.Body)
	pr.Body.Close()
	if pr.StatusCode != 409 {
		t.Fatalf("put secret = %d, want 409: %s", pr.StatusCode, body)
	}
	if !bytes.Contains(body, []byte("secret detected")) {
		t.Errorf("409 body missing the reason: %s", body)
	}

	// A clean file still goes through.
	cr := do(t, ts, "PUT", "/v1/sandboxes/"+sb.ID+"/files/work/ok.txt", token, "name = widget\nport = 8443\n")
	cr.Body.Close()
	if cr.StatusCode != 200 {
		t.Fatalf("put clean file = %d, want 200", cr.StatusCode)
	}

	// exec with a secret in an env value -> 409, command never runs.
	er := do(t, ts, "POST", "/v1/sandboxes/"+sb.ID+"/exec", token,
		`{"argv":["true"],"env":{"AWS_SECRET_ACCESS_KEY":"aZ2bYc4dXe6fWg8hVi0jUk2lTm4nSo6pRq8rQs0t"}}`)
	eb, _ := io.ReadAll(er.Body)
	er.Body.Close()
	if er.StatusCode != 409 {
		t.Fatalf("exec with secret env = %d, want 409: %s", er.StatusCode, eb)
	}
}

func TestAuditEndToEnd(t *testing.T) {
	dir := t.TempDir()
	ts, token := startServerWithAudit(t, dir)

	resp := do(t, ts, "POST", "/v1/sandboxes", token, `{"ttl_seconds":120}`)
	var sb struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&sb)
	resp.Body.Close()
	put := do(t, ts, "PUT", "/v1/sandboxes/"+sb.ID+"/files/work/x", token, "data")
	put.Body.Close()
	ex := do(t, ts, "POST", "/v1/sandboxes/"+sb.ID+"/exec", token, `{"argv":["/bin/true"],"timeout_ms":3000}`)
	io.Copy(io.Discard, ex.Body)
	ex.Body.Close()
	del := do(t, ts, "DELETE", "/v1/sandboxes/"+sb.ID, token, "")
	del.Body.Close()
	time.Sleep(400 * time.Millisecond)

	res, err := audit.Verify(dir)
	if err != nil || !res.OK {
		t.Fatalf("chain broken after a normal flow: %+v (%v)", res, err)
	}
	got, _ := audit.Query(dir, audit.Filter{Sandbox: sb.ID})
	var order []string
	for _, e := range got {
		order = append(order, e.Type)
	}
	// create before file.put before exec before destroy.
	idx := func(tp string) int {
		for i, v := range order {
			if v == tp {
				return i
			}
		}
		return -1
	}
	if !(idx(audit.TypeSandboxCreate) < idx(audit.TypeFilePut) &&
		idx(audit.TypeFilePut) < idx(audit.TypeExec) &&
		idx(audit.TypeExec) < idx(audit.TypeSandboxDestroy)) {
		t.Fatalf("event order wrong: %v", order)
	}

	m := do(t, ts, "GET", "/metrics", "", "")
	body, _ := io.ReadAll(m.Body)
	m.Body.Close()
	if !bytes.Contains(body, []byte("moat_audit_events_total")) {
		t.Fatal("/metrics missing moat_audit_events_total")
	}
}

// startServerAuditSyncToggle builds a server whose audit recorder fails its
// synchronous fsync while the returned flag is set. It returns the test server
// and that flag.
func startServerAuditSyncToggle(t *testing.T, dir string, tokens map[string]string,
	policies map[string]server.TokenPolicy, tokenMax int) (*httptest.Server, *atomic.Bool) {
	t.Helper()
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("no /dev/kvm")
	}
	m, err := config.NewMachine("srvtest-tpl")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(); err != nil {
		t.Skipf("images missing: %v", err)
	}
	log := logrus.New()
	log.SetLevel(logrus.WarnLevel)

	var failSync atomic.Bool
	rec, err := audit.New(audit.Config{
		Dir: dir, Metrics: audit.NopMetrics(), Log: log,
		FsyncBatch: 4, FsyncInterval: 20 * time.Millisecond,
		SyncFunc: func(f *os.File) error {
			if failSync.Load() {
				return errors.New("simulated fsync failure")
			}
			return f.Sync()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(server.Config{
		Templates: []server.Template{{Name: "default", Kernel: m.KernelImagePath, RootFS: copyRootFS(t, m.RootFSPath), VCPUs: 1, MemMiB: 256}},
		Tokens:    tokens,
		Policies:  policies,
		TokenMax:  tokenMax,
		Audit:     rec,
		Version:   "test",
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
		_ = rec.Close()
	})
	return ts, &failSync
}

// TestAuditFailClosedOnSyncError drives the 503 + rollback path with a
// deterministic fsync failure. A 201 must imply the sandbox.create event is on
// disk. When the fsync fails the create must roll back fully.
func TestAuditFailClosedOnSyncError(t *testing.T) {
	dir := t.TempDir()
	ts, failSync := startServerAuditSyncToggle(t, dir, map[string]string{"t1": "secret1"}, nil, 1)
	token := "secret1"

	// The next create cannot fsync its sandbox.create event.
	failSync.Store(true)
	resp := do(t, ts, "POST", "/v1/sandboxes", token, `{"ttl_seconds":60}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("create with a failing fsync = %d (%s), want 503", resp.StatusCode, body)
	}
	failSync.Store(false)

	// The sandbox is not in the registry.
	list := do(t, ts, "GET", "/v1/sandboxes", token, "")
	lb, _ := io.ReadAll(list.Body)
	list.Body.Close()
	if bytes.Contains(lb, []byte(`"id"`)) {
		t.Fatalf("a sandbox leaked past a failed audit write: %s", lb)
	}

	// The audit log holds no sandbox.create event.
	time.Sleep(200 * time.Millisecond)
	creates, _ := audit.Query(dir, audit.Filter{Types: []string{audit.TypeSandboxCreate}})
	if len(creates) != 0 {
		t.Fatalf("a sandbox.create event survived a failed fsync: %+v", creates)
	}

	// The token's one concurrency slot is free again, so a clean create works.
	time.Sleep(2 * time.Second)
	ok := do(t, ts, "POST", "/v1/sandboxes", token, `{"ttl_seconds":60}`)
	ob, _ := io.ReadAll(ok.Body)
	ok.Body.Close()
	if ok.StatusCode != http.StatusCreated {
		t.Fatalf("create after a rolled-back create = %d (%s), want 201", ok.StatusCode, ob)
	}
	var sb struct {
		ID string `json:"id"`
	}
	json.Unmarshal(ob, &sb)
	if sb.ID != "" {
		do(t, ts, "DELETE", "/v1/sandboxes/"+sb.ID, token, "").Body.Close()
	}
}

// TestSecretAllowFailsClosedOnSyncError checks that an allowed secret does not
// enter a sandbox when its audit record cannot fsync. The exec must not run.
func TestSecretAllowFailsClosedOnSyncError(t *testing.T) {
	dir := t.TempDir()
	ts, failSync := startServerAuditSyncToggle(t, dir,
		map[string]string{"priv": "s3cr3t"},
		map[string]server.TokenPolicy{"priv": {AllowSecretOverride: true}}, 4)
	token := "s3cr3t"

	resp := do(t, ts, "POST", "/v1/sandboxes", token, `{"ttl_seconds":120}`)
	var sb struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&sb)
	resp.Body.Close()
	if sb.ID == "" {
		t.Fatal("create returned no id")
	}
	t.Cleanup(func() { do(t, ts, "DELETE", "/v1/sandboxes/"+sb.ID, token, "").Body.Close() })

	failSync.Store(true)
	body := `{"argv":["/bin/true"],"env":{"AWS_SECRET_ACCESS_KEY":"aZ2bYc4dXe6fWg8hVi0jUk2lTm4nSo6pRq8rQs0t"},"allow_secrets":true}`
	er := do(t, ts, "POST", "/v1/sandboxes/"+sb.ID+"/exec", token, body)
	eb, _ := io.ReadAll(er.Body)
	er.Body.Close()
	failSync.Store(false)

	if er.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("exec with an allowed secret and a failing audit sync = %d (%s), want 503", er.StatusCode, eb)
	}
	if !bytes.Contains(eb, []byte("audit write failed")) {
		t.Fatalf("503 body missing the reason: %s", eb)
	}
	// The exec did not run: no NDJSON exit frame came back.
	if bytes.Contains(eb, []byte(`"stream":"exit"`)) {
		t.Fatalf("exec ran despite the 503: %s", eb)
	}
}

// TestAuditBlockedFilePutRecorded checks that a blocked PUT records a file.put
// event with scan "block" and zero bytes, on top of the secret.block event.
func TestAuditBlockedFilePutRecorded(t *testing.T) {
	dir := t.TempDir()
	ts, token := startServerWithAudit(t, dir)

	resp := do(t, ts, "POST", "/v1/sandboxes", token, `{"ttl_seconds":120}`)
	var sb struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&sb)
	resp.Body.Close()
	t.Cleanup(func() { do(t, ts, "DELETE", "/v1/sandboxes/"+sb.ID, token, "").Body.Close() })

	secretFile := "AWS_SECRET_ACCESS_KEY=aZ2bYc4dXe6fWg8hVi0jUk2lTm4nSo6pRq8rQs0t\n"
	pr := do(t, ts, "PUT", "/v1/sandboxes/"+sb.ID+"/files/work/.env", token, secretFile)
	pr.Body.Close()
	if pr.StatusCode != 409 {
		t.Fatalf("put secret = %d, want 409", pr.StatusCode)
	}

	time.Sleep(300 * time.Millisecond)
	evs, _ := audit.Query(dir, audit.Filter{})
	var sawBlock, sawFilePut bool
	for _, e := range evs {
		switch e.Type {
		case audit.TypeSecretBlock:
			if e.Attrs["target"] == "/work/.env" {
				sawBlock = true
			}
		case audit.TypeFilePut:
			if e.Sandbox != sb.ID {
				continue
			}
			sawFilePut = true
			if s, _ := e.Attrs["scan"].(string); s != "block" {
				t.Fatalf("blocked file.put scan = %q, want block", s)
			}
			if bv, _ := e.Attrs["bytes"].(float64); bv != 0 {
				t.Fatalf("blocked file.put bytes = %v, want 0", bv)
			}
			for _, bad := range []string{"content", "sha256", "value"} {
				if _, leak := e.Attrs[bad]; leak {
					t.Fatalf("blocked file.put leaked %q", bad)
				}
			}
		}
	}
	if !sawBlock || !sawFilePut {
		t.Fatalf("blocked PUT events: secret.block=%v file.put=%v", sawBlock, sawFilePut)
	}

	// The report lists the refused write.
	rep := do(t, ts, "GET", "/v1/sandboxes/"+sb.ID+"/report", token, "")
	var r struct {
		Files []struct {
			Path string `json:"path"`
			Scan string `json:"scan"`
		} `json:"files"`
	}
	json.NewDecoder(rep.Body).Decode(&r)
	rep.Body.Close()
	found := false
	for _, f := range r.Files {
		if f.Path == "/work/.env" && f.Scan == "block" {
			found = true
		}
	}
	if !found {
		t.Fatalf("report files do not list the blocked write: %+v", r.Files)
	}
}

// --- image verification (Task 7) ---------------------------------------------

func imgSHA256File(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// writeImagesLock writes a minimal valid images.lock beside rootfsPath whose
// digests match the real files.
func writeImagesLock(t *testing.T, kernelPath, rootfsPath string) string {
	t.Helper()
	ksum := imgSHA256File(t, kernelPath)
	rsum := imgSHA256File(t, rootfsPath)
	body := `{
  "schema": "moat.images.lock/v1",
  "source_date_epoch": 1788134400,
  "kernel": {"path":"vmlinux","sha256":"` + ksum + `","ci_snapshot":"s","version":"6.1.182","source":"http://x"},
  "rootfs": {"path":"rootfs.ext4","sha256":"` + rsum + `","size_bytes":1,"base_image":"alpine@sha256:x","fs_uuid":"u","mkfs_version":"m"},
  "sbom": {"path":"sbom.cdx.json","sha256":"deadbeef"},
  "guest_agent": {"sha256":"a","go":"go1.27.0"},
  "generated_by":"t","generated_at":"2026-08-31T00:00:00Z"
}`
	p := filepath.Join(filepath.Dir(rootfsPath), "images.lock")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(p) })
	return p
}

// copyImagesToTemp copies the real kernel and rootfs into a fresh dir so a test
// can place an images.lock beside them without touching build/artifacts.
func copyImagesToTemp(t *testing.T) (kernel, rootfs string) {
	t.Helper()
	m, err := config.NewMachine("imgverify-tpl")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(); err != nil {
		t.Skipf("images missing: %v", err)
	}
	dir := t.TempDir()
	kernel = filepath.Join(dir, "vmlinux")
	rootfs = filepath.Join(dir, "rootfs.ext4")
	for _, c := range []struct{ src, dst string }{
		{m.KernelImagePath, kernel}, {m.RootFSPath, rootfs},
	} {
		b, err := os.ReadFile(c.src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(c.dst, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return kernel, rootfs
}

func startServerImageVerify(t *testing.T, mode, kernel, rootfs string) (*server.Server, error) {
	t.Helper()
	log := logrus.New()
	log.SetLevel(logrus.ErrorLevel)
	return server.New(server.Config{
		Templates:   []server.Template{{Name: "default", Kernel: kernel, RootFS: rootfs, VCPUs: 1, MemMiB: 256}},
		Tokens:      map[string]string{"t1": "s1"},
		ImageVerify: mode,
		Version:     "test",
	}, log)
}

func TestImageVerifyEnforcePasses(t *testing.T) {
	kernel, rootfs := copyImagesToTemp(t)
	writeImagesLock(t, kernel, rootfs)
	srv, err := startServerImageVerify(t, "enforce", kernel, rootfs)
	if err != nil {
		t.Fatalf("enforce with a matching lock must start: %v", err)
	}
	t.Cleanup(func() {
		sc, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(sc)
	})
}

func TestImageVerifyMismatchRefuses(t *testing.T) {
	kernel, rootfs := copyImagesToTemp(t)
	writeImagesLock(t, kernel, rootfs)
	f, err := os.OpenFile(rootfs, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("tamper")
	f.Close()

	if _, err := startServerImageVerify(t, "enforce", kernel, rootfs); err == nil {
		t.Fatal("enforce with a digest mismatch must refuse to start")
	}
	srv, err := startServerImageVerify(t, "warn", kernel, rootfs)
	if err != nil {
		t.Fatalf("warn mode must start on a mismatch: %v", err)
	}
	sc, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(sc)
}

func TestImageVerifyNoLockStarts(t *testing.T) {
	kernel, rootfs := copyImagesToTemp(t)
	srv, err := startServerImageVerify(t, "enforce", kernel, rootfs)
	if err != nil {
		t.Fatalf("a missing lock must not stop startup: %v", err)
	}
	sc, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(sc)
}

// collectExec streams an exec NDJSON response and returns stdout+stderr joined
// and the exit code (-999 when no exit frame came back).
func collectExec(t *testing.T, resp *http.Response) (string, int) {
	t.Helper()
	var out string
	code := -999
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var ev map[string]any
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("bad NDJSON frame %q: %v", sc.Text(), err)
		}
		switch ev["stream"] {
		case "stdout", "stderr":
			d, _ := base64.StdEncoding.DecodeString(ev["data"].(string))
			out += string(d)
		case "exit":
			code = int(ev["code"].(float64))
		}
	}
	resp.Body.Close()
	return out, code
}

func startServerWithTemplate(t *testing.T, tpl server.Template) (*httptest.Server, string) {
	t.Helper()
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("no /dev/kvm")
	}
	m, err := config.NewMachine("srvtest-tmpldef")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(); err != nil {
		t.Skipf("images missing (run: make images): %v", err)
	}
	tpl.Kernel = m.KernelImagePath
	tpl.RootFS = copyRootFS(t, m.RootFSPath)
	if tpl.VCPUs == 0 {
		tpl.VCPUs = 1
	}
	if tpl.MemMiB == 0 {
		tpl.MemMiB = 256
	}
	log := logrus.New()
	log.SetLevel(logrus.WarnLevel)
	srv, err := server.New(server.Config{
		Templates: []server.Template{tpl},
		Tokens:    map[string]string{"t1": "secret1"},
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

func TestExecTemplateDefaults(t *testing.T) {
	ts, token := startServerWithTemplate(t, server.Template{
		Name:        "default",
		Env:         map[string]string{"MOAT_TMPL_VAR": "hello"},
		Dir:         "/tmp",
		DefaultArgv: []string{"echo", "default-cmd"},
	})

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
	t.Cleanup(func() { do(t, ts, "DELETE", "/v1/sandboxes/"+sb.ID, token, "").Body.Close() })

	// no dir, no argv override -> runs in /tmp and sees the template env
	out, code := collectExec(t, do(t, ts, "POST", "/v1/sandboxes/"+sb.ID+"/exec", token,
		`{"argv":["sh","-c","pwd; echo $MOAT_TMPL_VAR"]}`))
	if code != 0 || !strings.Contains(out, "/tmp") || !strings.Contains(out, "hello") {
		t.Fatalf("template defaults not applied: code=%d out=%q", code, out)
	}

	// request dir and env win
	out, _ = collectExec(t, do(t, ts, "POST", "/v1/sandboxes/"+sb.ID+"/exec", token,
		`{"argv":["sh","-c","pwd; echo $MOAT_TMPL_VAR"],"dir":"/","env":{"MOAT_TMPL_VAR":"override"}}`))
	if !strings.Contains(out, "override") || strings.Contains(out, "/tmp") {
		t.Fatalf("request values did not win: out=%q", out)
	}

	// empty argv -> the template default runs
	out, code = collectExec(t, do(t, ts, "POST", "/v1/sandboxes/"+sb.ID+"/exec", token, `{}`))
	if code != 0 || !strings.Contains(out, "default-cmd") {
		t.Fatalf("default_argv did not run: code=%d out=%q", code, out)
	}
}
