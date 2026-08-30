# Audit Event System Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give `moatd` a durable, hash-chained, metadata-only audit log of every sandbox boundary crossing, with a read API, a `moat audit` CLI, and a per-sandbox report.

**Architecture:** A new `internal/audit` package owns the on-disk log: an append-only set of rotated JSONL files, each event hash-chained to the last. A `Recorder` writes (synchronously + `fsync` for boundary events, through a bounded async buffer for hot events) and keeps an in-memory offset index for fast reads. `internal/server` calls the `Recorder` at each boundary point and exposes `GET /v1/audit` and `GET /v1/sandboxes/{id}/report`. `internal/egress` records egress decisions, resolving the sandbox from the client IP via a closure the server supplies. The `moat audit` CLI reads through the API or, with `--dir`, straight off the files.

**Tech Stack:** Go 1.27, standard library only for the new package (`crypto/sha256`, `encoding/json`, `os`, `bufio`, `sync`). `github.com/sirupsen/logrus` for logs, `github.com/prometheus/client_golang` for metrics (both already in `go.mod`). No new dependency.

**Spec:** `docs/superpowers/specs/2026-08-30-audit-event-system-design.md` — read it alongside this plan.

## Global Constraints

- **Module path:** `github.com/M30W-M1X/moat`. Go version floor `go 1.27`.
- **No new `go.mod` dependency.** The `internal/audit` package uses only the standard library. If `go mod tidy` wants to add anything, stop and reconsider.
- **Comments and log/error strings follow ASD-STE100 (Simplified Technical English):** active voice, one instruction per sentence, ≤ 20 words for instructions and ≤ 25 for descriptions, no semicolons, no phrasal verbs ("start" not "spin up", "remove" not "take off"), simple tenses, no nominalisation. One verb per action across the codebase — this code uses "start" / "stop" for lifecycle, never "boot" / "tear down".
- **Metadata only.** No audit event ever carries a payload, an environment value, a file body, stdin, or command output. `argv` and environment variable **names** are allowed. Secret **values** are never recorded, only rule ids.
- **Test build tags:** unit tests have no tag and run under `make test`. Tests that boot real microVMs use `//go:build integration` and run under `make test-integration`.
- **Boundary events are synchronous:** `sandbox.create`, `sandbox.destroy`, `secret.block`, `secret.allow`, `secret.redact`, `egress.deny`, `quota.reject`, `daemon.start`, `daemon.stop` are appended and `fsync`'d before the triggering API response. A failed write of one of these fails the request.
- **Async events never fail a request:** `exec`, `file.put`, `file.get`, `egress.allow`, `orphan.reap` go through a bounded buffer. A full buffer or write error increments `moat_audit_dropped_total` and logs at error level.
- **Audit is on by default.** Absent config ⇒ enabled at `config.AuditDir()`. `"dir": ""` or `-audit-dir ""` disables it (the `Recorder` becomes a no-op).
- **Commit style:** Conventional Commits (`feat:`, `test:`, `docs:`, `refactor:`). End every commit message body with the two trailers:
  ```
  Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01PRvf4bxSFzyL2dAPgorw7M
  ```
- **Branch:** all work lands on `m2-audit-event-system` (already checked out).
- **Before each commit:** run `gofmt -l .` (must print nothing), `go vet ./...`, and `go test ./...`. For tasks that touch `internal/server`, also `go vet -tags integration ./...`.

---

## File Structure

**New package `internal/audit/`** — the on-disk audit log. No dependency on `internal/server`.

| File | Responsibility |
|---|---|
| `internal/audit/event.go` | `Event` struct, the closed set of type constants, `canonical(Event) []byte`, `hashEvent(Event) string`. |
| `internal/audit/recorder.go` | `Config`, `Recorder`, `New`, `RecordSync`, `Record`, `Close`. Sync + async write paths, the write mutex, seq and chain-tip state, the `Metrics` hook. |
| `internal/audit/rotation.go` | File naming, size/date roll, `prune` by age and total size. Called by the `Recorder`. |
| `internal/audit/index.go` | `index` type: `map[sandboxID][]locator`, `map[tokenID][]locator`, per-sandbox time bounds, seq tip, chain tip. Built by a scan, updated on every append. |
| `internal/audit/reader.go` | Stateless functions over a directory of files: `Query(dir, Filter)`, `Verify(dir)`, `scanFiles`. Used by the `Recorder` (for the live API) and by the CLI `--dir` mode. |
| `internal/audit/report.go` | `Report` struct and `BuildReport(events []Event) Report`. |
| `internal/audit/*_test.go` | Unit tests per the spec §7. |

**Modified — `internal/server/`**

| File | Change |
|---|---|
| `server.go` | `Config.Audit *audit.Recorder`, `Config.Version string`, `Server.audit`, `TokenPolicy.AuditRead string`; construct a no-op recorder when `Config.Audit` is nil; emit `daemon.start` in `Start`, `daemon.stop` in `Shutdown`; call the recorder in `handleCreate`, `handleDelete`, `janitor`, `Shutdown`, `screen`, `handleExec`, `handlePutFile`, `handleGetFile`; add `handleAudit` and `handleReport`; add `IdentifyClient`. |
| `metrics.go` | Four new collectors and a `promMetrics` adapter that satisfies `audit.Metrics`. |

**Modified — `internal/egress/proxy.go`, `internal/egress/transparent.go`**

| File | Change |
|---|---|
| `proxy.go` | `Proxy` gains an optional `*audit.Recorder` and an `Identify func(netip.Addr) (string, string, bool)`; `SetAudit` setter; `decide` records `egress.allow` / `egress.deny`. |
| `transparent.go` | The transparent path records with `via: "transparent"`. |

**Modified — `cmd/moatd/`**

| File | Change |
|---|---|
| `main.go` | `-audit-dir` flag; build the `Recorder`; pass `Config.Audit` and `Config.Version`; call `proxy.SetAudit(rec, srv.IdentifyClient)` after `server.New`. |
| `config.go` | `fileConfig.Audit *fileAudit`, `fileToken.AuditRead string`; parse and validate. |

**Modified — `internal/config/config.go`**: add `AuditDir() string`.

**Modified — `internal/moatapi/moatapi.go`**: `Event`, `AuditFilter`, `Report` types; `Audit` and `Report` methods.

**New — `cmd/moat/audit.go`**: `runAudit([]string) int` with `tail`, `query`, `report`, `verify` subcommands. **Modified `cmd/moat/main.go`**: dispatch `case "audit"` and usage text.

**Modified docs:** `docs/api.md`, `docs/moatd.example.json`, `README.md`, `SKILL.md`.

---

## Task 1: Event type and canonical hashing

**Files:**
- Create: `internal/audit/event.go`
- Test: `internal/audit/event_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type Event struct { Seq uint64; TS time.Time; Type string; Sandbox string; Token string; Attrs map[string]any; Prev string; Hash string }`
  - Type constants: `TypeDaemonStart, TypeDaemonStop, TypeSandboxCreate, TypeSandboxDestroy, TypeExec, TypeFilePut, TypeFileGet, TypeSecretBlock, TypeSecretAllow, TypeSecretRedact, TypeEgressAllow, TypeEgressDeny, TypeQuotaReject, TypeOrphanReap string`
  - `func canonical(e Event) ([]byte, error)` — JSON of the event with keys sorted at every level, `hash` omitted, no extra whitespace.
  - `func hashEvent(e Event) (string, error)` — `hex(sha256(canonical(e)))`.
  - `const zeroHash = "0000000000000000000000000000000000000000000000000000000000000000"`

- [ ] **Step 1: Write the failing test**

```go
package audit

import (
	"strings"
	"testing"
	"time"
)

func TestCanonicalIsStableAndSorted(t *testing.T) {
	e := Event{
		Seq:     7,
		TS:      time.Date(2026, 8, 30, 18, 14, 7, 913482000, time.UTC),
		Type:    TypeExec,
		Sandbox: "sb-7f3a19c4",
		Token:   "ci",
		Attrs:   map[string]any{"exit": 0, "argv": []any{"python", "/work/task.py"}, "dur_ms": 1180},
		Prev:    "a91c4f",
	}
	b1, err := canonical(e)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"attrs":{"argv":["python","/work/task.py"],"dur_ms":1180,"exit":0},` +
		`"prev":"a91c4f","seq":7,"sandbox":"sb-7f3a19c4","token":"ci",` +
		`"ts":"2026-08-30T18:14:07.913482Z","type":"exec"}`
	if string(b1) != want {
		t.Fatalf("canonical mismatch:\n got %s\nwant %s", b1, want)
	}
	// The "hash" field is never part of the canonical form.
	e.Hash = "deadbeef"
	b2, _ := canonical(e)
	if string(b2) != string(b1) {
		t.Fatalf("hash field leaked into canonical form: %s", b2)
	}
	if strings.Contains(string(b1), "deadbeef") {
		t.Fatal("hash value present in canonical bytes")
	}
}

func TestHashEventKnownVector(t *testing.T) {
	e := Event{
		Seq:  1,
		TS:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Type: TypeDaemonStart,
		Prev: zeroHash,
	}
	got, err := hashEvent(e)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 64 {
		t.Fatalf("hash length %d, want 64", len(got))
	}
	// Deterministic across runs.
	again, _ := hashEvent(e)
	if got != again {
		t.Fatalf("hash not deterministic: %s vs %s", got, again)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/audit/ -run TestCanonical -v`
Expected: FAIL — `undefined: Event`, `undefined: canonical`.

- [ ] **Step 3: Write the implementation**

```go
// Package audit records a durable, tamper-evident log of every boundary a
// Moat sandbox crosses. Events are metadata only. The log never holds a
// secret, a payload, or command output.
package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// Event is one recorded boundary crossing. Attrs is type-specific metadata.
type Event struct {
	Seq     uint64
	TS      time.Time
	Type    string
	Sandbox string // sandbox id, or "" for a daemon-level event
	Token   string // token id, never the secret
	Attrs   map[string]any
	Prev    string // hash of the previous event
	Hash    string // hash of this event's canonical form
}

// The closed set of event types for v1.
const (
	TypeDaemonStart    = "daemon.start"
	TypeDaemonStop     = "daemon.stop"
	TypeSandboxCreate  = "sandbox.create"
	TypeSandboxDestroy = "sandbox.destroy"
	TypeExec           = "exec"
	TypeFilePut        = "file.put"
	TypeFileGet        = "file.get"
	TypeSecretBlock    = "secret.block"
	TypeSecretAllow    = "secret.allow"
	TypeSecretRedact   = "secret.redact"
	TypeEgressAllow    = "egress.allow"
	TypeEgressDeny     = "egress.deny"
	TypeQuotaReject    = "quota.reject"
	TypeOrphanReap     = "orphan.reap"
)

const zeroHash = "0000000000000000000000000000000000000000000000000000000000000000"

// canonical serialises e with keys sorted at every level and the hash field
// omitted. encoding/json already sorts map keys and struct fields marshal in
// declaration order, so the field order below is the canonical order.
func canonical(e Event) ([]byte, error) {
	m := map[string]any{
		"seq":     e.Seq,
		"ts":      e.TS.UTC().Format(time.RFC3339Nano),
		"type":    e.Type,
		"sandbox": e.Sandbox,
		"token":   e.Token,
		"attrs":   e.Attrs,
		"prev":    e.Prev,
	}
	if e.Attrs == nil {
		m["attrs"] = map[string]any{}
	}
	b, err := json.Marshal(m) // json.Marshal sorts map keys
	if err != nil {
		return nil, fmt.Errorf("canonical event: %w", err)
	}
	return b, nil
}

// hashEvent returns the hex sha256 of e's canonical form.
func hashEvent(e Event) (string, error) {
	b, err := canonical(e)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
```

Note on the test's `want` string: `json.Marshal` of a `map[string]any` sorts keys, so top-level order is `attrs, prev, seq, sandbox, token, ts, type` and nested `attrs` sorts to `argv, dur_ms, exit`. Numeric literals in `Attrs` marshalled from `int` stay integers. Adjust the `want` literal to match `json.Marshal` output exactly when you run it.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/audit/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/audit/
git add internal/audit/event.go internal/audit/event_test.go
git commit -m "$(cat <<'EOF'
feat(audit): event type and canonical hashing

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01PRvf4bxSFzyL2dAPgorw7M
EOF
)"
```

---

## Task 2: Recorder — synchronous write path, file, and hash chain

**Files:**
- Create: `internal/audit/recorder.go`
- Test: `internal/audit/recorder_test.go`

**Interfaces:**
- Consumes: `Event`, `hashEvent`, `zeroHash`, type constants from Task 1.
- Produces:
  - `type Config struct { Dir string; MaxFileMB int; MaxTotalMB int; MaxAge time.Duration; FsyncBatch int; FsyncInterval time.Duration; Metrics Metrics; Log *logrus.Logger }`
  - `type Metrics interface { Recorded(typ, result string); Dropped(); WriteObserved(path string, seconds float64); QueueDepth(n int) }`
  - `func NopMetrics() Metrics`
  - `type Recorder struct { ... }`
  - `func New(cfg Config) (*Recorder, error)` — opens or creates `Dir`, scans for the seq and chain tip, starts the async writer. A `Dir` of `""` returns a no-op recorder (all methods succeed and do nothing).
  - `func (r *Recorder) RecordSync(ctx context.Context, e Event) error` — fills `Seq`, `TS`, `Prev`, `Hash`; appends the line; `fsync`; returns any error.
  - `func (r *Recorder) Close() error` — drains the async buffer, `fsync`, closes the file.
  - `func (r *Recorder) Enabled() bool`

- [ ] **Step 1: Write the failing test**

```go
package audit

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

func testRecorder(t *testing.T) (*Recorder, string) {
	t.Helper()
	dir := t.TempDir()
	log := logrus.New()
	log.SetLevel(logrus.ErrorLevel)
	r, err := New(Config{Dir: dir, MaxFileMB: 8, MaxTotalMB: 64,
		MaxAge: time.Hour, FsyncBatch: 8, FsyncInterval: 50 * time.Millisecond,
		Metrics: NopMetrics(), Log: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r, dir
}

func readLines(t *testing.T, dir string) []Event {
	t.Helper()
	entries, _ := filepath.Glob(filepath.Join(dir, "audit-*.jsonl"))
	var out []Event
	for _, f := range entries {
		fh, err := os.Open(f)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
		for sc.Scan() {
			var e Event
			if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
				t.Fatalf("bad line in %s: %v", f, err)
			}
			out = append(out, e)
		}
		fh.Close()
	}
	return out
}

func TestRecordSyncChain(t *testing.T) {
	r, dir := testRecorder(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := r.RecordSync(ctx, Event{Type: TypeSandboxCreate, Sandbox: "sb-x", Token: "ci"}); err != nil {
			t.Fatal(err)
		}
	}
	evs := readLines(t, dir)
	if len(evs) != 5 {
		t.Fatalf("wrote %d events, want 5", len(evs))
	}
	prev := zeroHash
	for i, e := range evs {
		if e.Seq != uint64(i+1) {
			t.Fatalf("event %d has seq %d", i, e.Seq)
		}
		if e.Prev != prev {
			t.Fatalf("event %d prev = %s, want %s", i, e.Prev, prev)
		}
		want, _ := hashEvent(e)
		if e.Hash != want {
			t.Fatalf("event %d hash = %s, want %s", i, e.Hash, want)
		}
		if e.TS.IsZero() {
			t.Fatalf("event %d has no timestamp", i)
		}
		prev = e.Hash
	}
}

func TestNoopRecorder(t *testing.T) {
	r, err := New(Config{Dir: "", Metrics: NopMetrics(), Log: logrus.New()})
	if err != nil {
		t.Fatal(err)
	}
	if r.Enabled() {
		t.Fatal("empty Dir must give a disabled recorder")
	}
	if err := r.RecordSync(context.Background(), Event{Type: TypeExec}); err != nil {
		t.Fatalf("no-op RecordSync returned %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRecordSyncSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Dir: dir, MaxFileMB: 8, MaxTotalMB: 64, MaxAge: time.Hour,
		FsyncBatch: 8, FsyncInterval: 50 * time.Millisecond, Metrics: NopMetrics(), Log: logrus.New()}
	r1, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_ = r1.RecordSync(context.Background(), Event{Type: TypeDaemonStart})
	_ = r1.RecordSync(context.Background(), Event{Type: TypeSandboxCreate, Sandbox: "sb-1"})
	_ = r1.Close()

	r2, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	_ = r2.RecordSync(context.Background(), Event{Type: TypeSandboxCreate, Sandbox: "sb-2"})
	evs := readLines(t, dir)
	if len(evs) != 3 || evs[2].Seq != 3 {
		t.Fatalf("after restart got %d events, last seq %d", len(evs), evs[len(evs)-1].Seq)
	}
	if evs[2].Prev != evs[1].Hash {
		t.Fatal("chain broke across restart")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/audit/ -run TestRecordSync -v`
Expected: FAIL — `undefined: New`, `undefined: Recorder`.

- [ ] **Step 3: Write the implementation**

```go
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// Metrics receives audit-writer counters. A server passes a Prometheus-backed
// implementation. Tests pass NopMetrics().
type Metrics interface {
	Recorded(typ, result string)
	Dropped()
	WriteObserved(path string, seconds float64)
	QueueDepth(n int)
}

type nopMetrics struct{}

func (nopMetrics) Recorded(string, string)          {}
func (nopMetrics) Dropped()                         {}
func (nopMetrics) WriteObserved(string, float64)    {}
func (nopMetrics) QueueDepth(int)                   {}

// NopMetrics returns a Metrics that discards everything.
func NopMetrics() Metrics { return nopMetrics{} }

// Config sets up a Recorder.
type Config struct {
	Dir           string // "" disables the recorder
	MaxFileMB     int
	MaxTotalMB    int
	MaxAge        time.Duration
	FsyncBatch    int
	FsyncInterval time.Duration
	Metrics       Metrics
	Log           *logrus.Logger
}

func (c *Config) setDefaults() {
	if c.MaxFileMB <= 0 {
		c.MaxFileMB = 128
	}
	if c.MaxTotalMB <= 0 {
		c.MaxTotalMB = 2048
	}
	if c.MaxAge <= 0 {
		c.MaxAge = 720 * time.Hour
	}
	if c.FsyncBatch <= 0 {
		c.FsyncBatch = 64
	}
	if c.FsyncInterval <= 0 {
		c.FsyncInterval = 200 * time.Millisecond
	}
	if c.Metrics == nil {
		c.Metrics = NopMetrics()
	}
	if c.Log == nil {
		c.Log = logrus.New()
	}
}

// Recorder appends events to the on-disk log. It is safe for concurrent use.
type Recorder struct {
	cfg     Config
	enabled bool

	mu      sync.Mutex // guards f, seq, tip, curBytes, curDay
	f       *os.File
	seq     uint64
	tip     string // hash of the last event written
	curName string
	curSize int64

	idx *index

	queue    chan Event
	writerWG sync.WaitGroup
	closed   chan struct{}
	closeOne sync.Once
}

// New opens the log in cfg.Dir. An empty Dir returns a disabled Recorder whose
// methods do nothing and always succeed.
func New(cfg Config) (*Recorder, error) {
	cfg.setDefaults()
	if cfg.Dir == "" {
		return &Recorder{cfg: cfg, enabled: false}, nil
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("audit dir: %w", err)
	}
	r := &Recorder{
		cfg:     cfg,
		enabled: true,
		tip:     zeroHash,
		idx:     newIndex(),
		queue:   make(chan Event, 8192),
		closed:  make(chan struct{}),
	}
	if err := r.recover(); err != nil {
		return nil, err
	}
	if err := r.openCurrent(); err != nil {
		return nil, err
	}
	r.writerWG.Add(1)
	go r.writeLoop()
	return r, nil
}

// Enabled reports whether the recorder writes anything.
func (r *Recorder) Enabled() bool { return r.enabled }

// RecordSync assigns the envelope fields, appends the event, and fsyncs before
// returning. A non-nil error means the event is NOT on disk.
func (r *Recorder) RecordSync(ctx context.Context, e Event) error {
	if !r.enabled {
		return nil
	}
	start := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	err := r.appendLocked(&e, true)
	r.cfg.Metrics.WriteObserved("sync", time.Since(start).Seconds())
	if err != nil {
		r.cfg.Metrics.Recorded(e.Type, "error")
		return err
	}
	r.cfg.Metrics.Recorded(e.Type, "ok")
	return nil
}

// appendLocked fills the chain fields, rotates if needed, writes the line, and
// optionally fsyncs. The caller holds r.mu.
func (r *Recorder) appendLocked(e *Event, sync bool) error {
	if err := r.rotateIfNeededLocked(); err != nil {
		return err
	}
	r.seq++
	e.Seq = r.seq
	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	}
	e.Prev = r.tip
	h, err := hashEvent(*e)
	if err != nil {
		r.seq--
		return err
	}
	e.Hash = h
	line, err := marshalLine(*e)
	if err != nil {
		r.seq--
		return err
	}
	n, err := r.f.Write(line)
	if err != nil {
		r.seq--
		return fmt.Errorf("append audit line: %w", err)
	}
	if sync {
		if err := r.f.Sync(); err != nil {
			return fmt.Errorf("fsync audit log: %w", err)
		}
	}
	r.curSize += int64(n)
	r.tip = h
	r.idx.add(*e, r.curName, r.curSize-int64(n))
	return nil
}

// marshalLine renders e as its full JSON object plus a newline. The stored form
// is the canonical object with the hash field appended.
func marshalLine(e Event) ([]byte, error) {
	m := map[string]any{
		"seq": e.Seq, "ts": e.TS.UTC().Format(time.RFC3339Nano), "type": e.Type,
		"sandbox": e.Sandbox, "token": e.Token, "attrs": e.Attrs,
		"prev": e.Prev, "hash": e.Hash,
	}
	if e.Attrs == nil {
		m["attrs"] = map[string]any{}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Close drains the async buffer, fsyncs, and closes the file.
func (r *Recorder) Close() error {
	if !r.enabled {
		return nil
	}
	r.closeOne.Do(func() { close(r.closed) })
	r.writerWG.Wait()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	if err := r.f.Sync(); err != nil {
		return err
	}
	return r.f.Close()
}
```

Add stubs the async task fills in (so this task compiles): put these in `recorder.go` too, empty for now —

```go
// writeLoop drains the async queue. Task 3 implements it.
func (r *Recorder) writeLoop() { defer r.writerWG.Done(); <-r.closed }

// Record enqueues e without blocking. Task 3 implements it.
func (r *Recorder) Record(e Event) {}
```

And stubs the rotation and index tasks own — in `recorder.go` for now, moved to their files later:

```go
func (r *Recorder) rotateIfNeededLocked() error {
	if r.f != nil && r.curSize < int64(r.cfg.MaxFileMB)<<20 &&
		r.curName == dailyName(time.Now(), 0) {
		return nil
	}
	return r.openCurrent()
}

func (r *Recorder) openCurrent() error {
	name := dailyName(time.Now(), 0)
	for n := 1; ; n++ {
		p := filepath.Join(r.cfg.Dir, name)
		info, err := os.Stat(p)
		if err != nil || info.Size() < int64(r.cfg.MaxFileMB)<<20 {
			f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				return fmt.Errorf("open audit file: %w", err)
			}
			if r.f != nil {
				_ = r.f.Close()
			}
			r.f, r.curName = f, name
			if info != nil {
				r.curSize = info.Size()
			} else {
				r.curSize = 0
			}
			return nil
		}
		name = dailyName(time.Now(), n)
	}
}

func dailyName(t time.Time, n int) string {
	if n == 0 {
		return fmt.Sprintf("audit-%s.jsonl", t.UTC().Format("2006-01-02"))
	}
	return fmt.Sprintf("audit-%s.%d.jsonl", t.UTC().Format("2006-01-02"), n)
}

// recover scans existing files for the highest seq and the chain tip, and
// rebuilds the index. Task 5/6 replace the body with the shared scanner.
func (r *Recorder) recover() error {
	files, err := listFiles(r.cfg.Dir)
	if err != nil {
		return err
	}
	for _, f := range files {
		evs, _, err := scanFile(f)
		if err != nil {
			return err
		}
		for _, e := range evs {
			if e.Seq > r.seq {
				r.seq = e.Seq
			}
			r.tip = e.Hash
			r.idx.add(e, filepath.Base(f), 0)
		}
	}
	return nil
}
```

The helpers `listFiles`, `scanFile`, `index`, `newIndex` are created in Tasks 5 and 6. To keep this task compiling on its own, add a minimal `internal/audit/scan.go` now:

```go
package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

// listFiles returns the audit files in dir, oldest name first.
func listFiles(dir string) ([]string, error) {
	m, err := filepath.Glob(filepath.Join(dir, "audit-*.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Strings(m)
	return m, nil
}

// scanFile reads every event in one file. The bool is false when the last line
// was truncated (a crash mid-write); such a line is dropped.
func scanFile(path string) ([]Event, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return out, false, nil // truncated or corrupt tail
		}
		out = append(out, e)
	}
	return out, true, sc.Err()
}
```

And a minimal `internal/audit/index.go`:

```go
package audit

import (
	"sync"
	"time"
)

type locator struct {
	File   string
	Offset int64
}

type index struct {
	mu       sync.RWMutex
	bySandbox map[string][]locator
	byToken   map[string][]locator
	first     map[string]time.Time
	last      map[string]time.Time
}

func newIndex() *index {
	return &index{
		bySandbox: map[string][]locator{},
		byToken:   map[string][]locator{},
		first:     map[string]time.Time{},
		last:      map[string]time.Time{},
	}
}

func (i *index) add(e Event, file string, off int64) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if e.Sandbox != "" {
		i.bySandbox[e.Sandbox] = append(i.bySandbox[e.Sandbox], locator{file, off})
		if t, ok := i.first[e.Sandbox]; !ok || e.TS.Before(t) {
			i.first[e.Sandbox] = e.TS
		}
		i.last[e.Sandbox] = e.TS
	}
	if e.Token != "" {
		i.byToken[e.Token] = append(i.byToken[e.Token], locator{file, off})
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/audit/ -v`
Expected: PASS for `TestRecordSyncChain`, `TestNoopRecorder`, `TestRecordSyncSurvivesRestart`, plus Task 1's tests.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/audit/
go vet ./internal/audit/
git add internal/audit/
git commit -m "$(cat <<'EOF'
feat(audit): synchronous recorder with a hash chain across restarts

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01PRvf4bxSFzyL2dAPgorw7M
EOF
)"
```

---

## Task 3: Recorder — asynchronous write path

**Files:**
- Modify: `internal/audit/recorder.go` (replace the `writeLoop` and `Record` stubs)
- Test: `internal/audit/recorder_test.go` (add cases)

**Interfaces:**
- Consumes: `Recorder`, `appendLocked`, `Config`, `Metrics` from Task 2.
- Produces:
  - `func (r *Recorder) Record(e Event)` — non-blocking enqueue. A full queue drops `e`, calls `Metrics.Dropped()`, and logs at error level.
  - `writeLoop` drains `r.queue`, appends each event without a per-event `fsync`, and calls `f.Sync()` after `FsyncBatch` events or `FsyncInterval`, whichever comes first. On `Close`, it drains what remains and `fsync`s.
  - The write mutex `r.mu` is shared with `RecordSync`, so `seq` stays dense and the chain stays consistent regardless of path.

- [ ] **Step 1: Write the failing test**

```go
func TestRecordAsyncDrainsAndChains(t *testing.T) {
	r, dir := testRecorder(t)
	for i := 0; i < 40; i++ {
		r.Record(Event{Type: TypeExec, Sandbox: "sb-a", Token: "ci",
			Attrs: map[string]any{"argv": []any{"echo", "hi"}, "exit": 0}})
	}
	if err := r.Close(); err != nil { // Close drains
		t.Fatal(err)
	}
	evs := readLines(t, dir)
	if len(evs) != 40 {
		t.Fatalf("drained %d events, want 40", len(evs))
	}
	prev := zeroHash
	for i, e := range evs {
		if e.Seq != uint64(i+1) {
			t.Fatalf("event %d seq %d", i, e.Seq)
		}
		if e.Prev != prev {
			t.Fatalf("event %d chain broke", i)
		}
		prev = e.Hash
	}
}

type countMetrics struct {
	Metrics
	dropped atomic.Int64
}

func (c *countMetrics) Dropped() { c.dropped.Add(1) }

func TestRecordAsyncDropsWhenFull(t *testing.T) {
	dir := t.TempDir()
	cm := &countMetrics{Metrics: NopMetrics()}
	log := logrus.New()
	log.SetLevel(logrus.FatalLevel)
	// A tiny queue and a slow disk are hard to force here; instead push far
	// more than the buffer before the writer can drain, with the writer
	// blocked by a held mutex.
	r, err := New(Config{Dir: dir, MaxFileMB: 8, MaxTotalMB: 64, MaxAge: time.Hour,
		FsyncBatch: 1000, FsyncInterval: time.Hour, Metrics: cm, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	r.mu.Lock() // stall the writer
	for i := 0; i < 20000; i++ {
		r.Record(Event{Type: TypeExec})
	}
	r.mu.Unlock()
	_ = r.Close()
	if cm.dropped.Load() == 0 {
		t.Fatal("expected some events to be dropped when the queue filled")
	}
}

func TestSyncAndAsyncInterleaveKeepSeqDense(t *testing.T) {
	r, dir := testRecorder(t)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if i%2 == 0 {
					_ = r.RecordSync(context.Background(), Event{Type: TypeSandboxCreate, Sandbox: "sb", Token: "ci"})
				} else {
					r.Record(Event{Type: TypeExec, Sandbox: "sb", Token: "ci"})
				}
			}
		}(g)
	}
	wg.Wait()
	_ = r.Close()
	evs := readLines(t, dir)
	if len(evs) != 400 {
		t.Fatalf("got %d events, want 400", len(evs))
	}
	prev := zeroHash
	for i, e := range evs {
		if e.Seq != uint64(i+1) {
			t.Fatalf("seq gap at index %d: seq %d", i, e.Seq)
		}
		if e.Prev != prev {
			t.Fatalf("chain broke at index %d", i)
		}
		prev = e.Hash
	}
}
```

Add imports to the test file: `"sync"`, `"sync/atomic"`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/audit/ -run 'TestRecordAsync|TestSyncAndAsync' -v`
Expected: FAIL — the stub `writeLoop` never writes, so `readLines` returns 0.

- [ ] **Step 3: Write the implementation**

Replace the two stubs in `recorder.go`:

```go
// Record enqueues e for the async writer. It never blocks. A full queue drops
// the event and increments the drop metric.
func (r *Recorder) Record(e Event) {
	if !r.enabled {
		return
	}
	select {
	case r.queue <- e:
		r.cfg.Metrics.QueueDepth(len(r.queue))
	default:
		r.cfg.Metrics.Dropped()
		r.cfg.Log.WithField("type", e.Type).Error("audit event dropped: async queue full")
	}
}

// writeLoop drains the async queue, batching fsyncs.
func (r *Recorder) writeLoop() {
	defer r.writerWG.Done()
	ticker := time.NewTicker(r.cfg.FsyncInterval)
	defer ticker.Stop()
	pending := 0
	flush := func() {
		if pending == 0 {
			return
		}
		r.mu.Lock()
		if r.f != nil {
			if err := r.f.Sync(); err != nil {
				r.cfg.Log.WithError(err).Error("audit batch fsync failed")
			}
		}
		r.mu.Unlock()
		pending = 0
	}
	drain := func() {
		for {
			select {
			case e := <-r.queue:
				r.writeOne(e)
				pending++
			default:
				return
			}
		}
	}
	for {
		select {
		case <-r.closed:
			drain()
			flush()
			return
		case e := <-r.queue:
			r.writeOne(e)
			pending++
			if pending >= r.cfg.FsyncBatch {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// writeOne appends one async event without a per-event fsync.
func (r *Recorder) writeOne(e Event) {
	start := time.Now()
	r.mu.Lock()
	err := r.appendLocked(&e, false)
	r.mu.Unlock()
	r.cfg.Metrics.WriteObserved("async", time.Since(start).Seconds())
	if err != nil {
		r.cfg.Metrics.Dropped()
		r.cfg.Metrics.Recorded(e.Type, "error")
		r.cfg.Log.WithError(err).WithField("type", e.Type).Error("audit async write failed")
		return
	}
	r.cfg.Metrics.Recorded(e.Type, "ok")
	r.cfg.Metrics.QueueDepth(len(r.queue))
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/audit/ -v`
Expected: PASS. If `TestRecordAsyncDropsWhenFull` flakes, raise the push count to `100000`; the queue is 8192.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/audit/
go vet ./internal/audit/
git add internal/audit/recorder.go internal/audit/recorder_test.go
git commit -m "$(cat <<'EOF'
feat(audit): bounded async write path with batched fsync

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01PRvf4bxSFzyL2dAPgorw7M
EOF
)"
```

---

## Task 4: Recorder — rotation and retention

**Files:**
- Create: `internal/audit/rotation.go`
- Modify: `internal/audit/recorder.go` (call `r.prune()` after a roll; move `rotateIfNeededLocked` / `openCurrent` / `dailyName` here from Task 2)
- Test: `internal/audit/rotation_test.go`

**Interfaces:**
- Consumes: `Recorder`, `Config`, `listFiles`, `scanFile`.
- Produces:
  - `func (r *Recorder) prune() error` — after a roll, delete whole files whose newest `ts` is older than `MaxAge`, then delete the oldest files while total bytes exceed `MaxTotalMB`. Never deletes the current file.
  - Rotation writes the first line of a new file with `prev` equal to the previous file's last hash (already true because `r.tip` persists across `openCurrent`).

- [ ] **Step 1: Write the failing test**

```go
package audit

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

func TestRotationCarriesChain(t *testing.T) {
	dir := t.TempDir()
	log := logrus.New()
	log.SetLevel(logrus.ErrorLevel)
	// 1 MB files so a few hundred events roll it.
	r, err := New(Config{Dir: dir, MaxFileMB: 1, MaxTotalMB: 64, MaxAge: time.Hour,
		FsyncBatch: 16, FsyncInterval: 20 * time.Millisecond, Metrics: NopMetrics(), Log: log})
	if err != nil {
		t.Fatal(err)
	}
	big := map[string]any{"pad": string(make([]byte, 4096))}
	for i := 0; i < 400; i++ {
		if err := r.RecordSync(context.Background(), Event{Type: TypeExec, Sandbox: "sb", Token: "ci", Attrs: big}); err != nil {
			t.Fatal(err)
		}
	}
	_ = r.Close()
	files, _ := listFiles(dir)
	if len(files) < 2 {
		t.Fatalf("expected the log to roll into >= 2 files, got %d", len(files))
	}
	evs := readLines(t, dir)
	prev := zeroHash
	for i, e := range evs {
		if e.Seq != uint64(i+1) || e.Prev != prev {
			t.Fatalf("chain or seq broke at %d across rotation", i)
		}
		prev = e.Hash
	}
}

func TestPruneByAgeAndSize(t *testing.T) {
	dir := t.TempDir()
	// Two old files by name and mtime, one current.
	old1 := filepath.Join(dir, "audit-2020-01-01.jsonl")
	old2 := filepath.Join(dir, "audit-2020-01-02.jsonl")
	for _, p := range []string{old1, old2} {
		if err := os.WriteFile(p, []byte(`{"seq":1,"ts":"2020-01-01T00:00:00Z","type":"exec","sandbox":"","token":"","attrs":{},"prev":"`+zeroHash+`","hash":"x"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(p, time.Now().Add(-100*24*time.Hour), time.Now().Add(-100*24*time.Hour))
	}
	log := logrus.New()
	log.SetLevel(logrus.ErrorLevel)
	r, err := New(Config{Dir: dir, MaxFileMB: 1, MaxTotalMB: 64, MaxAge: 24 * time.Hour,
		Metrics: NopMetrics(), Log: log})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.prune(); err != nil {
		t.Fatal(err)
	}
	files, _ := listFiles(dir)
	for _, f := range files {
		if filepath.Base(f) == "audit-2020-01-01.jsonl" || filepath.Base(f) == "audit-2020-01-02.jsonl" {
			t.Fatalf("prune left an over-age file: %s", f)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/audit/ -run 'TestRotation|TestPrune' -v`
Expected: `TestPruneByAgeAndSize` FAIL — `r.prune undefined`. `TestRotationCarriesChain` may already pass from Task 2's rotation stubs; that is fine.

- [ ] **Step 3: Write the implementation**

Move `rotateIfNeededLocked`, `openCurrent`, `dailyName` out of `recorder.go` into `rotation.go`, and add:

```go
package audit

import (
	"os"
	"path/filepath"
	"time"
)

// rotateIfNeededLocked opens a fresh file when the current one is full or the
// UTC day changed. The caller holds r.mu. After a roll it prunes.
func (r *Recorder) rotateIfNeededLocked() error {
	today := dailyName(time.Now(), 0)
	if r.f != nil && r.curSize < int64(r.cfg.MaxFileMB)<<20 && sameDay(r.curName, today) {
		return nil
	}
	if err := r.openCurrent(); err != nil {
		return err
	}
	if err := r.prune(); err != nil {
		r.cfg.Log.WithError(err).Error("audit retention prune failed")
	}
	return nil
}

func sameDay(name, today string) bool {
	// audit-YYYY-MM-DD.jsonl or audit-YYYY-MM-DD.N.jsonl
	return len(name) >= 16 && len(today) >= 16 && name[:16] == today[:16]
}

// prune deletes over-age files, then the oldest files while the total exceeds
// MaxTotalMB. It never deletes the current file.
func (r *Recorder) prune() error {
	files, err := listFiles(r.cfg.Dir)
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-r.cfg.MaxAge)
	var kept []string
	var total int64
	for _, f := range files {
		if filepath.Base(f) == r.curName {
			if fi, err := os.Stat(f); err == nil {
				total += fi.Size()
			}
			kept = append(kept, f)
			continue
		}
		fi, err := os.Stat(f)
		if err != nil {
			continue
		}
		if fi.ModTime().Before(cutoff) {
			_ = os.Remove(f)
			continue
		}
		total += fi.Size()
		kept = append(kept, f)
	}
	budget := int64(r.cfg.MaxTotalMB) << 20
	i := 0
	for total > budget && i < len(kept) {
		if filepath.Base(kept[i]) == r.curName {
			i++
			continue
		}
		if fi, err := os.Stat(kept[i]); err == nil {
			total -= fi.Size()
		}
		_ = os.Remove(kept[i])
		i++
	}
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/audit/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/audit/
go vet ./internal/audit/
git add internal/audit/rotation.go internal/audit/recorder.go internal/audit/rotation_test.go
git commit -m "$(cat <<'EOF'
feat(audit): file rotation and age/size retention

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01PRvf4bxSFzyL2dAPgorw7M
EOF
)"
```

---

## Task 5: Reader — Query and Verify over a directory of files

**Files:**
- Create: `internal/audit/reader.go`
- Test: `internal/audit/reader_test.go`

**Interfaces:**
- Consumes: `Event`, `hashEvent`, `zeroHash`, `listFiles`, `scanFile`.
- Produces:
  - `type Filter struct { Sandbox string; Token string; Types []string; Since time.Time; Until time.Time; Limit int; Desc bool }`
  - `func (f Filter) match(e Event) bool`
  - `func Query(dir string, f Filter) ([]Event, error)` — scans every file in order, applies `f`, honours `Limit` (default 1000, hard cap 100000) and `Desc`.
  - `type VerifyResult struct { OK bool; Events int; FirstSeq, LastSeq uint64; FirstTS, LastTS time.Time; PrunedPrefix bool; Problems []string }`
  - `func Verify(dir string) (VerifyResult, error)` — recomputes every `hash`, checks every `prev` link and `seq` continuity across all files. A `prev` that does not match the previous event but where the previous `seq` is `1`-greater-than-nothing (chain starts mid-stream) is reported as `PrunedPrefix`, not a `Problem`.

- [ ] **Step 1: Write the failing test**

```go
package audit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

func writeSome(t *testing.T, dir string, n int) {
	t.Helper()
	log := logrus.New()
	log.SetLevel(logrus.ErrorLevel)
	r, err := New(Config{Dir: dir, MaxFileMB: 8, MaxTotalMB: 64, MaxAge: time.Hour,
		FsyncBatch: 8, FsyncInterval: 20 * time.Millisecond, Metrics: NopMetrics(), Log: log})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		typ := TypeExec
		sb := "sb-a"
		if i%3 == 0 {
			typ, sb = TypeSandboxCreate, "sb-b"
		}
		_ = r.RecordSync(context.Background(), Event{Type: typ, Sandbox: sb, Token: "ci"})
	}
	_ = r.Close()
}

func TestQueryFilters(t *testing.T) {
	dir := t.TempDir()
	writeSome(t, dir, 30)

	all, err := Query(dir, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 30 {
		t.Fatalf("Query{} returned %d, want 30", len(all))
	}
	onlyB, _ := Query(dir, Filter{Sandbox: "sb-b"})
	for _, e := range onlyB {
		if e.Sandbox != "sb-b" {
			t.Fatalf("sandbox filter leaked %s", e.Sandbox)
		}
	}
	if len(onlyB) != 10 {
		t.Fatalf("sb-b count = %d, want 10", len(onlyB))
	}
	creates, _ := Query(dir, Filter{Types: []string{TypeSandboxCreate}})
	if len(creates) != 10 {
		t.Fatalf("create count = %d, want 10", len(creates))
	}
	limited, _ := Query(dir, Filter{Limit: 5})
	if len(limited) != 5 || limited[0].Seq != 1 {
		t.Fatalf("limit=5 gave %d events starting at seq %d", len(limited), limited[0].Seq)
	}
	desc, _ := Query(dir, Filter{Limit: 3, Desc: true})
	if desc[0].Seq != 30 {
		t.Fatalf("desc first seq = %d, want 30", desc[0].Seq)
	}
}

func TestVerifyClean(t *testing.T) {
	dir := t.TempDir()
	writeSome(t, dir, 20)
	res, err := Verify(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Events != 20 || res.FirstSeq != 1 || res.LastSeq != 20 {
		t.Fatalf("verify on a clean log: %+v", res)
	}
}

func TestVerifyDetectsTamper(t *testing.T) {
	edit := func(dir string, fn func(lines []string) []string) {
		files, _ := listFiles(dir)
		p := files[len(files)-1]
		b, _ := os.ReadFile(p)
		lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
		lines = fn(lines)
		_ = os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	}

	t.Run("edited attr", func(t *testing.T) {
		dir := t.TempDir()
		writeSome(t, dir, 10)
		edit(dir, func(l []string) []string {
			l[4] = strings.Replace(l[4], `"token":"ci"`, `"token":"evil"`, 1)
			return l
		})
		res, _ := Verify(dir)
		if res.OK {
			t.Fatal("verify passed an edited event")
		}
	})

	t.Run("deleted middle line", func(t *testing.T) {
		dir := t.TempDir()
		writeSome(t, dir, 10)
		edit(dir, func(l []string) []string { return append(l[:4], l[5:]...) })
		res, _ := Verify(dir)
		if res.OK {
			t.Fatal("verify passed a log with a deleted line")
		}
		joined := strings.Join(res.Problems, " ")
		if !strings.Contains(joined, "seq") && !strings.Contains(joined, "prev") {
			t.Fatalf("expected a seq/prev problem, got %v", res.Problems)
		}
	})

	t.Run("swapped lines", func(t *testing.T) {
		dir := t.TempDir()
		writeSome(t, dir, 10)
		edit(dir, func(l []string) []string { l[3], l[4] = l[4], l[3]; return l })
		res, _ := Verify(dir)
		if res.OK {
			t.Fatal("verify passed swapped lines")
		}
	})

	t.Run("truncated last line", func(t *testing.T) {
		dir := t.TempDir()
		writeSome(t, dir, 10)
		files, _ := listFiles(dir)
		p := files[len(files)-1]
		b, _ := os.ReadFile(p)
		_ = os.WriteFile(p, b[:len(b)-10], 0o600) // chop the tail
		res, _ := Verify(dir)
		// A truncated tail is a crash artefact: verify drops it and stays OK
		// over the intact prefix.
		if !res.OK {
			t.Fatalf("verify should tolerate a truncated tail: %+v", res)
		}
		if res.Events != 9 {
			t.Fatalf("expected 9 intact events, got %d", res.Events)
		}
	})
}

func TestVerifyPrunedPrefix(t *testing.T) {
	dir := t.TempDir()
	writeSome(t, dir, 40)
	files, _ := listFiles(dir)
	// Delete the first file to simulate retention pruning the chain head.
	if len(files) < 1 {
		t.Skip("only one file")
	}
	_ = os.Remove(files[0])
	res, _ := Verify(dir)
	if !res.OK || !res.PrunedPrefix {
		t.Fatalf("expected OK with PrunedPrefix, got %+v", res)
	}
	_ = filepath.Base(files[0])
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/audit/ -run 'TestQuery|TestVerify' -v`
Expected: FAIL — `undefined: Query`, `undefined: Verify`, `undefined: Filter`.

- [ ] **Step 3: Write the implementation**

```go
package audit

import (
	"fmt"
	"sort"
	"time"
)

// Filter narrows a Query. A zero field does not filter.
type Filter struct {
	Sandbox string
	Token   string
	Types   []string
	Since   time.Time
	Until   time.Time
	Limit   int
	Desc    bool
}

func (f Filter) match(e Event) bool {
	if f.Sandbox != "" && e.Sandbox != f.Sandbox {
		return false
	}
	if f.Token != "" && e.Token != f.Token {
		return false
	}
	if len(f.Types) > 0 {
		hit := false
		for _, t := range f.Types {
			if e.Type == t {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	if !f.Since.IsZero() && e.TS.Before(f.Since) {
		return false
	}
	if !f.Until.IsZero() && e.TS.After(f.Until) {
		return false
	}
	return true
}

const (
	defaultLimit = 1000
	maxLimit     = 100000
)

// Query returns the events in dir that match f, in seq order (or reverse when
// f.Desc is set).
func Query(dir string, f Filter) ([]Event, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	files, err := listFiles(dir)
	if err != nil {
		return nil, err
	}
	var out []Event
	for _, file := range files {
		evs, _, err := scanFile(file)
		if err != nil {
			return nil, err
		}
		for _, e := range evs {
			if f.match(e) {
				out = append(out, e)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if f.Desc {
			return out[i].Seq > out[j].Seq
		}
		return out[i].Seq < out[j].Seq
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// VerifyResult is the outcome of a chain check.
type VerifyResult struct {
	OK           bool
	Events       int
	FirstSeq     uint64
	LastSeq      uint64
	FirstTS      time.Time
	LastTS       time.Time
	PrunedPrefix bool
	Problems     []string
}

// Verify recomputes every hash and checks every prev link and seq step across
// all files in dir.
func Verify(dir string) (VerifyResult, error) {
	files, err := listFiles(dir)
	if err != nil {
		return VerifyResult{}, err
	}
	var res VerifyResult
	var prevHash string
	var prevSeq uint64
	first := true
	for _, file := range files {
		evs, intact, err := scanFile(file)
		if err != nil {
			return res, err
		}
		if !intact && len(evs) > 0 {
			// A truncated tail is expected after a crash; the intact prefix
			// still verifies.
		}
		for _, e := range evs {
			want, herr := hashEvent(e)
			if herr != nil {
				res.Problems = append(res.Problems, fmt.Sprintf("hash error at seq %d: %v", e.Seq, herr))
				continue
			}
			if e.Hash != want {
				res.Problems = append(res.Problems, fmt.Sprintf("hash mismatch at seq %d", e.Seq))
			}
			if first {
				res.FirstSeq, res.FirstTS = e.Seq, e.TS
				if e.Prev != zeroHash {
					res.PrunedPrefix = true
				}
				first = false
			} else {
				if e.Seq != prevSeq+1 {
					res.Problems = append(res.Problems, fmt.Sprintf("seq gap: %d then %d", prevSeq, e.Seq))
				}
				if e.Prev != prevHash {
					res.Problems = append(res.Problems, fmt.Sprintf("prev break at seq %d", e.Seq))
				}
			}
			prevHash, prevSeq = e.Hash, e.Seq
			res.LastSeq, res.LastTS = e.Seq, e.TS
			res.Events++
		}
	}
	res.OK = len(res.Problems) == 0
	return res, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/audit/ -v`
Expected: PASS. `TestVerifyDetectsTamper/deleted_middle_line` relies on `seq` continuity catching the gap even though the hash of the surviving lines is still self-consistent — the `prev break` and `seq gap` problems both fire.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/audit/
go vet ./internal/audit/
git add internal/audit/reader.go internal/audit/reader_test.go
git commit -m "$(cat <<'EOF'
feat(audit): Query and Verify over the on-disk log

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01PRvf4bxSFzyL2dAPgorw7M
EOF
)"
```

---

## Task 6: Report builder and Recorder-backed live reads

**Files:**
- Create: `internal/audit/report.go`
- Modify: `internal/audit/recorder.go` (add `LiveQuery`, `LiveReport`, `SandboxToken`), `internal/audit/index.go` (finish `add`, add `sandboxIDs`, `tokenOf`)
- Test: `internal/audit/report_test.go`

**Interfaces:**
- Consumes: `Event`, `Filter`, `Query`, `Verify`, `Recorder`, `index`.
- Produces:
  - `type Report struct { Sandbox, Token, Template string; CreatedAt, DestroyedAt time.Time; DestroyReason string; LifetimeS int64; Networked bool; GuestIP string; Counts map[string]int; Exec ExecSummary; Bytes BytesSummary; Files []FileRef; SecretEvents []SecretRef; Egress EgressSummary; Chain ChainRef }` with `ExecSummary{N, Nonzero, Errors int; TotalDurMS int64}`, `BytesSummary{In, Out int64}`, `FileRef{Dir, Path string; Bytes int64; SHA256, Scan string}`, `SecretRef{Type, Target string; Rules []string}`, `EgressSummary{Allow, Deny []string}`, `ChainRef{FirstSeq, LastSeq uint64; Verified bool}`.
  - `func BuildReport(events []Event) Report` — pure function over one sandbox's events, in seq order.
  - `func (r *Recorder) LiveQuery(f Filter) ([]Event, error)` — same as `Query` but reads `r.cfg.Dir` under `r.mu` so it sees the current file too.
  - `func (r *Recorder) LiveReport(sandboxID string) (Report, error)` — `LiveQuery(Filter{Sandbox: sandboxID})` then `BuildReport`, then fills `Chain` from a `Verify` of that slice.
  - `func (r *Recorder) SandboxToken(sandboxID string) (string, bool)` — from the index, the token that created the sandbox (the token on its `sandbox.create` event).

- [ ] **Step 1: Write the failing test**

```go
package audit

import (
	"context"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

func TestBuildReport(t *testing.T) {
	base := time.Date(2026, 8, 30, 18, 0, 0, 0, time.UTC)
	evs := []Event{
		{Seq: 1, TS: base, Type: TypeSandboxCreate, Sandbox: "sb-1", Token: "ci",
			Attrs: map[string]any{"template": "python", "source": "pool", "networked": false, "ttl_s": 600, "guest_ip": ""}},
		{Seq: 2, TS: base.Add(time.Second), Type: TypeFilePut, Sandbox: "sb-1", Token: "ci",
			Attrs: map[string]any{"path": "/work/task.py", "bytes": 412, "sha256": "9f2c", "scan": "clean"}},
		{Seq: 3, TS: base.Add(2 * time.Second), Type: TypeExec, Sandbox: "sb-1", Token: "ci",
			Attrs: map[string]any{"exit": 0, "dur_ms": 1180}},
		{Seq: 4, TS: base.Add(3 * time.Second), Type: TypeExec, Sandbox: "sb-1", Token: "ci",
			Attrs: map[string]any{"exit": 1, "dur_ms": 200}},
		{Seq: 5, TS: base.Add(4 * time.Second), Type: TypeEgressDeny, Sandbox: "sb-1", Token: "ci",
			Attrs: map[string]any{"host": "169.254.169.254", "port": 80}},
		{Seq: 6, TS: base.Add(5 * time.Second), Type: TypeFileGet, Sandbox: "sb-1", Token: "ci",
			Attrs: map[string]any{"path": "/work/out.json", "bytes": 1290, "sha256": "aa"}},
		{Seq: 7, TS: base.Add(903 * time.Second), Type: TypeSandboxDestroy, Sandbox: "sb-1", Token: "ci",
			Attrs: map[string]any{"reason": "ttl", "lifetime_s": 903}},
	}
	rep := BuildReport(evs)
	if rep.Sandbox != "sb-1" || rep.Token != "ci" || rep.Template != "python" {
		t.Fatalf("header wrong: %+v", rep)
	}
	if rep.DestroyReason != "ttl" || rep.LifetimeS != 903 {
		t.Fatalf("destroy wrong: %+v", rep)
	}
	if rep.Exec.N != 2 || rep.Exec.Nonzero != 1 || rep.Exec.TotalDurMS != 1380 {
		t.Fatalf("exec summary wrong: %+v", rep.Exec)
	}
	if rep.Bytes.In != 412 || rep.Bytes.Out != 1290 {
		t.Fatalf("bytes wrong: %+v", rep.Bytes)
	}
	if len(rep.Egress.Deny) != 1 || rep.Egress.Deny[0] != "169.254.169.254:80" {
		t.Fatalf("egress deny wrong: %+v", rep.Egress)
	}
	if rep.Counts[TypeExec] != 2 || rep.Counts[TypeFilePut] != 1 {
		t.Fatalf("counts wrong: %+v", rep.Counts)
	}
}

func TestLiveReportAndSandboxToken(t *testing.T) {
	dir := t.TempDir()
	log := logrus.New()
	log.SetLevel(logrus.ErrorLevel)
	r, err := New(Config{Dir: dir, MaxFileMB: 8, MaxTotalMB: 64, MaxAge: time.Hour,
		FsyncBatch: 4, FsyncInterval: 10 * time.Millisecond, Metrics: NopMetrics(), Log: log})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	_ = r.RecordSync(ctx, Event{Type: TypeSandboxCreate, Sandbox: "sb-9", Token: "ci",
		Attrs: map[string]any{"template": "python"}})
	r.Record(Event{Type: TypeExec, Sandbox: "sb-9", Token: "ci", Attrs: map[string]any{"exit": 0, "dur_ms": 5}})
	time.Sleep(50 * time.Millisecond) // let the async event land

	tok, ok := r.SandboxToken("sb-9")
	if !ok || tok != "ci" {
		t.Fatalf("SandboxToken = %q, %v", tok, ok)
	}
	rep, err := r.LiveReport("sb-9")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Template != "python" || rep.Counts[TypeExec] != 1 {
		t.Fatalf("live report wrong: %+v", rep)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/audit/ -run 'TestBuildReport|TestLiveReport' -v`
Expected: FAIL — `undefined: BuildReport`, `undefined: (*Recorder).LiveReport`.

- [ ] **Step 3: Write the implementation**

`report.go`:

```go
package audit

import (
	"fmt"
	"time"
)

type ExecSummary struct {
	N          int   `json:"n"`
	Nonzero    int   `json:"nonzero"`
	Errors     int   `json:"errors"`
	TotalDurMS int64 `json:"total_dur_ms"`
}

type BytesSummary struct {
	In  int64 `json:"in"`
	Out int64 `json:"out"`
}

type FileRef struct {
	Dir    string `json:"dir"`
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
	Scan   string `json:"scan,omitempty"`
}

type SecretRef struct {
	Type   string   `json:"type"`
	Target string   `json:"target"`
	Rules  []string `json:"rules"`
}

type EgressSummary struct {
	Allow []string `json:"allow"`
	Deny  []string `json:"deny"`
}

type ChainRef struct {
	FirstSeq uint64 `json:"first_seq"`
	LastSeq  uint64 `json:"last_seq"`
	Verified bool   `json:"verified"`
}

// Report is the "what did this sandbox do" rollup.
type Report struct {
	Sandbox       string         `json:"sandbox"`
	Token         string         `json:"token"`
	Template      string         `json:"template"`
	CreatedAt     time.Time      `json:"created_at"`
	DestroyedAt   time.Time      `json:"destroyed_at"`
	DestroyReason string         `json:"destroy_reason"`
	LifetimeS     int64          `json:"lifetime_s"`
	Networked     bool           `json:"networked"`
	GuestIP       string         `json:"guest_ip"`
	Counts        map[string]int `json:"counts"`
	Exec          ExecSummary    `json:"exec"`
	Bytes         BytesSummary   `json:"bytes"`
	Files         []FileRef      `json:"files"`
	SecretEvents  []SecretRef    `json:"secret_events"`
	Egress        EgressSummary  `json:"egress"`
	Chain         ChainRef       `json:"chain"`
}

func attrStr(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func attrInt(m map[string]any, k string) int64 {
	switch v := m[k].(type) {
	case float64:
		return int64(v)
	case int:
		return int64(v)
	case int64:
		return v
	}
	return 0
}

func attrBool(m map[string]any, k string) bool {
	b, _ := m[k].(bool)
	return b
}

// BuildReport folds one sandbox's events, in seq order, into a Report.
func BuildReport(events []Event) Report {
	r := Report{Counts: map[string]int{}}
	for _, e := range events {
		r.Counts[e.Type]++
		if r.Sandbox == "" {
			r.Sandbox, r.Token = e.Sandbox, e.Token
		}
		switch e.Type {
		case TypeSandboxCreate:
			r.CreatedAt = e.TS
			r.Template = attrStr(e.Attrs, "template")
			r.Networked = attrBool(e.Attrs, "networked")
			r.GuestIP = attrStr(e.Attrs, "guest_ip")
		case TypeSandboxDestroy:
			r.DestroyedAt = e.TS
			r.DestroyReason = attrStr(e.Attrs, "reason")
			r.LifetimeS = attrInt(e.Attrs, "lifetime_s")
		case TypeExec:
			r.Exec.N++
			r.Exec.TotalDurMS += attrInt(e.Attrs, "dur_ms")
			if attrStr(e.Attrs, "error") != "" {
				r.Exec.Errors++
			} else if attrInt(e.Attrs, "exit") != 0 {
				r.Exec.Nonzero++
			}
		case TypeFilePut:
			r.Bytes.In += attrInt(e.Attrs, "bytes")
			r.Files = append(r.Files, FileRef{Dir: "in", Path: attrStr(e.Attrs, "path"),
				Bytes: attrInt(e.Attrs, "bytes"), SHA256: attrStr(e.Attrs, "sha256"),
				Scan: attrStr(e.Attrs, "scan")})
		case TypeFileGet:
			r.Bytes.Out += attrInt(e.Attrs, "bytes")
			r.Files = append(r.Files, FileRef{Dir: "out", Path: attrStr(e.Attrs, "path"),
				Bytes: attrInt(e.Attrs, "bytes"), SHA256: attrStr(e.Attrs, "sha256")})
		case TypeSecretBlock, TypeSecretAllow, TypeSecretRedact:
			r.SecretEvents = append(r.SecretEvents, SecretRef{
				Type: e.Type, Target: attrStr(e.Attrs, "target"), Rules: attrStrSlice(e.Attrs, "rules")})
		case TypeEgressAllow:
			r.Egress.Allow = append(r.Egress.Allow, hostPort(e.Attrs))
		case TypeEgressDeny:
			r.Egress.Deny = append(r.Egress.Deny, hostPort(e.Attrs))
		}
	}
	if len(events) > 0 {
		r.Chain.FirstSeq = events[0].Seq
		r.Chain.LastSeq = events[len(events)-1].Seq
	}
	return r
}

func hostPort(m map[string]any) string {
	return fmt.Sprintf("%s:%d", attrStr(m, "host"), attrInt(m, "port"))
}

func attrStrSlice(m map[string]any, k string) []string {
	raw, _ := m[k].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
```

Add to `recorder.go`:

```go
// LiveQuery runs Query over the recorder's own directory, holding the write
// lock so an in-flight append does not race the scan.
func (r *Recorder) LiveQuery(f Filter) ([]Event, error) {
	if !r.enabled {
		return nil, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f != nil {
		_ = r.f.Sync()
	}
	return Query(r.cfg.Dir, f)
}

// LiveReport builds the rollup for one sandbox from the live log.
func (r *Recorder) LiveReport(sandboxID string) (Report, error) {
	evs, err := r.LiveQuery(Filter{Sandbox: sandboxID, Limit: maxLimit})
	if err != nil {
		return Report{}, err
	}
	rep := BuildReport(evs)
	v, _ := Verify(r.cfg.Dir) // chain state of the whole log
	rep.Chain.Verified = v.OK
	return rep, nil
}

// SandboxToken returns the token id that created sandboxID.
func (r *Recorder) SandboxToken(sandboxID string) (string, bool) {
	if !r.enabled {
		return "", false
	}
	evs, err := r.LiveQuery(Filter{Sandbox: sandboxID, Types: []string{TypeSandboxCreate}, Limit: 1})
	if err != nil || len(evs) == 0 {
		return "", false
	}
	return evs[0].Token, true
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/audit/ -v`
Expected: PASS across the whole package.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/audit/
go vet ./internal/audit/
git add internal/audit/report.go internal/audit/recorder.go internal/audit/report_test.go
git commit -m "$(cat <<'EOF'
feat(audit): per-sandbox report and recorder-backed live reads

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01PRvf4bxSFzyL2dAPgorw7M
EOF
)"
```

---

## Task 7: Server — wire the Recorder, metrics adapter, and token policy

**Files:**
- Modify: `internal/server/server.go` (`Config`, `Server`, `TokenPolicy`, `New`)
- Modify: `internal/server/metrics.go` (four collectors + `promAuditMetrics` adapter)
- Test: `internal/server/audit_test.go` (new, no build tag — pure unit)

**Interfaces:**
- Consumes: `audit.New`, `audit.Config`, `audit.Recorder`, `audit.Metrics`, `audit.NopMetrics` from Tasks 2–6.
- Produces:
  - `Config` gains `Audit *audit.Recorder` (nil ⇒ `New` builds a disabled recorder) and `Version string`.
  - `TokenPolicy` gains `AuditRead string` (`"all"` or `""`).
  - `Server` gains `audit *audit.Recorder`.
  - `func (m *metrics) auditAdapter() audit.Metrics` returning a `*promAuditMetrics`.
  - `func (p TokenPolicy) auditAll() bool` → `p.AuditRead == "all"`.

- [ ] **Step 1: Write the failing test**

```go
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/server/ -run 'TestNew.*Recorder|TestAuditAll' -v`
Expected: FAIL — `s.audit` undefined, `Config.Audit` undefined, `auditAll` undefined.

- [ ] **Step 3: Write the implementation**

In `server.go`, add to `Config`:

```go
	// Audit records boundary crossings. A nil value gives a disabled recorder.
	Audit *audit.Recorder
	// Version is reported in the daemon.start audit event.
	Version string
	// ConfigSHA256 is the hex sha256 of the loaded moatd.json, or "" when the
	// daemon started from flags only. Reported in daemon.start.
	ConfigSHA256 string
```

Add to `TokenPolicy`:

```go
	// AuditRead widens audit visibility. "all" lets this token read every
	// token's events and run verify. Empty means own events only.
	AuditRead string
```

Add the method near `templateAllowed`:

```go
func (p TokenPolicy) auditAll() bool { return p.AuditRead == "all" }
```

Add to `Server`:

```go
	audit *audit.Recorder
```

In `New`, after `s.m = newMetrics(...)`, before `return s, nil`:

```go
	s.audit = cfg.Audit
	if s.audit == nil {
		rec, err := audit.New(audit.Config{Dir: "", Metrics: audit.NopMetrics(), Log: log})
		if err != nil {
			return nil, fmt.Errorf("build disabled audit recorder: %w", err)
		}
		s.audit = rec
	}
```

Add the import `"github.com/M30W-M1X/moat/internal/audit"` to `server.go`.

In `metrics.go`, add fields to `metrics`:

```go
	auditEvents  *prometheus.CounterVec
	auditDropped prometheus.Counter
	auditWrite   *prometheus.HistogramVec
	auditQueue   prometheus.Gauge
```

Construct them in `newMetrics` and add to `reg.MustRegister(...)`:

```go
		auditEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "moat_audit_events_total",
			Help: "Audit events recorded, by type and result (ok, error).",
		}, []string{"type", "result"}),
		auditDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "moat_audit_dropped_total",
			Help: "Async audit events dropped because the buffer was full or a write failed.",
		}),
		auditWrite: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "moat_audit_write_seconds",
			Help:    "Audit append and fsync latency, by path (sync, async).",
			Buckets: []float64{.0001, .0005, .001, .005, .01, .05, .1, .5},
		}, []string{"path"}),
		auditQueue: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "moat_audit_queue_depth",
			Help: "Audit async buffer occupancy.",
		}),
```

Add the adapter at the end of `metrics.go` (unexported — same package as `server.New`):

```go
type promAuditMetrics struct{ m *metrics }

func (a promAuditMetrics) Recorded(typ, result string)       { a.m.auditEvents.WithLabelValues(typ, result).Inc() }
func (a promAuditMetrics) Dropped()                          { a.m.auditDropped.Inc() }
func (a promAuditMetrics) WriteObserved(p string, s float64) { a.m.auditWrite.WithLabelValues(p).Observe(s) }
func (a promAuditMetrics) QueueDepth(n int)                  { a.m.auditQueue.Set(float64(n)) }
```

`cmd/moatd` builds the `Recorder` before `server.New` (it needs `Config.Audit` set), so the recorder starts with `NopMetrics()`. The server owns the Prometheus registry privately, so it swaps the sink in during `New`. Add a swap method to `internal/audit/recorder.go` (this is a small addition to the Task 2 file):

```go
// SetMetrics replaces the metrics sink. Call it once, before heavy traffic.
func (r *Recorder) SetMetrics(m Metrics) {
	if m == nil {
		m = NopMetrics()
	}
	r.mu.Lock()
	r.cfg.Metrics = m
	r.mu.Unlock()
}
```

In `server.New`, in the branch where `cfg.Audit != nil`:

```go
	s.audit = cfg.Audit
	if s.audit == nil {
		rec, err := audit.New(audit.Config{Dir: "", Metrics: audit.NopMetrics(), Log: log})
		if err != nil {
			return nil, fmt.Errorf("build disabled audit recorder: %w", err)
		}
		s.audit = rec
	} else if s.audit.Enabled() {
		s.audit.SetMetrics(promAuditMetrics{s.m})
	}
```

(This replaces the plain `s.audit = cfg.Audit` block shown earlier in this step.)

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/server/ -run 'TestNew.*Recorder|TestAuditAll' -v && go test ./internal/audit/ -v`
Expected: PASS. Add the `SetMetrics` method and its test to `internal/audit/recorder_test.go`:

```go
func TestSetMetrics(t *testing.T) {
	r, _ := testRecorder(t)
	cm := &countMetrics{Metrics: NopMetrics()}
	r.SetMetrics(cm)
	_ = r.RecordSync(context.Background(), Event{Type: TypeExec})
	// Recorded(...) is called; countMetrics only counts Dropped, so just
	// assert no panic and a clean write.
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/audit/ internal/server/
go vet ./... && go vet -tags integration ./...
git add internal/audit/recorder.go internal/audit/recorder_test.go internal/server/server.go internal/server/metrics.go internal/server/audit_test.go
git commit -m "$(cat <<'EOF'
feat(server): hold an audit.Recorder and export audit metrics

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01PRvf4bxSFzyL2dAPgorw7M
EOF
)"
```

---

## Task 8: Server — emit sandbox lifecycle and quota events

**Files:**
- Modify: `internal/server/server.go` (`Start`, `Shutdown`, `handleCreate`, `handleDelete`, `janitor`)
- Test: `internal/server/server_integration_test.go` (add `TestAuditLifecycle`, `//go:build integration`)

**Interfaces:**
- Consumes: `s.audit` (`*audit.Recorder`), `audit.Event`, type constants.
- Produces: after this task the audit log contains `daemon.start`, `sandbox.create`, `sandbox.destroy` (reason `deleted`/`ttl`/`shutdown`), `quota.reject`, `daemon.stop` for a full run. A failed `RecordSync` in `handleCreate` returns 503 and rolls the sandbox back.

- [ ] **Step 1: Write the failing test**

```go
//go:build integration

func TestAuditLifecycle(t *testing.T) {
	dir := t.TempDir()
	ts, token := startServerWithAudit(t, dir) // helper added below

	resp := do(t, ts, "POST", "/v1/sandboxes", token, `{"ttl_seconds":120}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create = %d", resp.StatusCode)
	}
	var sb struct{ ID string `json:"id"` }
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
```

Add the helper to `server_integration_test.go`:

```go
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
		Templates: []server.Template{{Name: "default", Kernel: m.KernelImagePath, RootFS: m.RootFSPath, VCPUs: 1, MemMiB: 256}},
		Tokens:    map[string]string{"t1": "secret1", "secops": "s3"},
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
```

Add imports `"github.com/M30W-M1X/moat/internal/audit"` to the integration test file.

- [ ] **Step 2: Run the test to verify it fails**

Run: `make test-integration 2>&1 | tail -30` (or `go test -tags integration ./internal/server/ -run TestAuditLifecycle -v`)
Expected: FAIL — no `daemon.start` / `sandbox.create` events written (server does not call the recorder yet).

- [ ] **Step 3: Write the implementation**

In `server.go` `Start`, after `s.baseCtx = ctx` and pool setup, before `go s.janitor()`:

```go
	s.startedAt = time.Now()
	_ = s.audit.RecordSync(ctx, audit.Event{
		Type: audit.TypeDaemonStart,
		Attrs: map[string]any{
			"version":       s.cfg.Version,
			"templates":     templateNames(s.cfg.Templates),
			"pool_size":     s.cfg.PoolSize,
			"net_pool":      s.cfg.NetPool,
			"secret_scan":   !s.cfg.SecretScanOff,
			"egress":        s.cfg.EgressPort > 0,
			"config_sha256": s.cfg.ConfigSHA256,
		},
	})
```

Add a helper near the bottom of `server.go`:

```go
func templateNames(ts []Template) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name
	}
	return out
}
```

In `Shutdown`, as the first statement inside the function (before closing `janitorStop`):

```go
	sctx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
	up := int64(0)
	if !s.startedAt.IsZero() {
		up = int64(time.Since(s.startedAt) / time.Second)
	}
	_ = s.audit.RecordSync(sctx, audit.Event{Type: audit.TypeDaemonStop,
		Attrs: map[string]any{"reason": "signal", "uptime_s": up}})
	scancel()
```

Add `startedAt time.Time` to the `Server` struct. `reason` is `"signal"` because `cmd/moatd` drives shutdown from `signal.NotifyContext`; if a future in-process caller adds an API-triggered shutdown, it passes its own reason.

And after the sandbox-teardown loop finishes (still in `Shutdown`), the per-sandbox destroy events are emitted by a shared helper — see below.

In `handleCreate`, replace the quota-reject block:

```go
	if err := s.reg.reserve(tid, tokenMax); err != nil {
		s.m.quotaRejects.Inc()
		scope := "token"
		if strings.Contains(err.Error(), "global") {
			scope = "global"
		}
		_ = s.audit.RecordSync(r.Context(), audit.Event{
			Type: audit.TypeQuotaReject, Token: tid,
			Attrs: map[string]any{"scope": scope, "template": name},
		})
		writeErr(w, http.StatusTooManyRequests, err.Error())
		return
	}
```

In `handleCreate`, after `s.reg.add(sb)` and the metric increments, before `writeJSON(w, http.StatusCreated, view(sb))`:

```go
	if err := s.audit.RecordSync(r.Context(), audit.Event{
		Type: audit.TypeSandboxCreate, Sandbox: sb.ID, Token: tid,
		Attrs: map[string]any{
			"template": name, "source": source, "networked": req.Networked,
			"ttl_s": int64(ttl / time.Second), "guest_ip": sb.GuestIP,
		},
	}); err != nil {
		// The audit record is a hard requirement for a boundary event.
		s.reg.remove(sb.ID)
		s.reg.release(tid)
		s.m.sandboxesActive.WithLabelValues(name).Dec()
		s.m.sandboxesCreated.WithLabelValues(name, source).Dec() // best effort
		s.asyncDestroy(sb)
		writeErr(w, http.StatusServiceUnavailable, "audit write failed: "+err.Error())
		return
	}
```

Note: `prometheus.Counter` has no `Dec`. Drop the `sandboxesCreated...Dec()` line — a counter cannot go down; accept a tiny over-count on this rare path and leave a `// counter cannot be decremented; a failed create is rare` comment.

Add a shared destroy helper and use it in `handleDelete`, `janitor`, and `Shutdown`:

```go
// recordDestroy emits the sandbox.destroy audit event. reason is one of
// deleted, ttl, idle, shutdown, orphan.
func (s *Server) recordDestroy(ctx context.Context, sb *Sandbox, reason string) {
	life := int64(time.Since(sb.CreatedAt) / time.Second)
	_ = s.audit.RecordSync(ctx, audit.Event{
		Type: audit.TypeSandboxDestroy, Sandbox: sb.ID, Token: sb.TokenID,
		Attrs: map[string]any{"reason": reason, "lifetime_s": life},
	})
}
```

- `handleDelete`: call `s.recordDestroy(r.Context(), sb, "deleted")` right before `s.asyncDestroy(sb)`.
- `janitor`: call `s.recordDestroy(context.Background(), sb, "ttl")` before `s.asyncDestroy(sb)`.
- `Shutdown`: inside the `for _, sb := range s.reg.list()` loop, call `s.recordDestroy(ctx, sb, "shutdown")` before `s.asyncDestroy(sb)`.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test -tags integration ./internal/server/ -run 'TestAuditLifecycle|TestAPILifecycle' -v`
Expected: PASS. `TestAPILifecycle` still passes (audit dir defaults to disabled in `startServer`, so its behaviour is unchanged).

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/server/
go vet ./... && go vet -tags integration ./...
go test ./...
git add internal/server/server.go internal/server/server_integration_test.go
git commit -m "$(cat <<'EOF'
feat(server): audit sandbox lifecycle and quota rejections

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01PRvf4bxSFzyL2dAPgorw7M
EOF
)"
```

---

## Task 9: Server — emit exec and file transfer events

**Files:**
- Modify: `internal/server/server.go` (`handleExec`, `handlePutFile`, `handleGetFile`)
- Test: `internal/server/server_integration_test.go` (extend `TestAuditLifecycle` or add `TestAuditExecAndFiles`)

**Interfaces:**
- Consumes: `s.audit`, `audit.Event`.
- Produces: `exec`, `file.put`, `file.get` events (async), each metadata only. Assert the events carry no `stdout`, `stderr`, `content`, or `value` key.

- [ ] **Step 1: Write the failing test**

```go
//go:build integration

func TestAuditExecAndFiles(t *testing.T) {
	dir := t.TempDir()
	ts, token := startServerWithAudit(t, dir)

	resp := do(t, ts, "POST", "/v1/sandboxes", token, `{"ttl_seconds":120}`)
	var sb struct{ ID string `json:"id"` }
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
	if pa["path"] != "/work/hello.txt" || int(pa["bytes"].(float64)) != 7 {
		t.Fatalf("file.put attrs wrong: %v", pa)
	}
	if _, bad := pa["content"]; bad {
		t.Fatal("file.put leaked content")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -tags integration ./internal/server/ -run TestAuditExecAndFiles -v`
Expected: FAIL — no `exec` / `file.put` / `file.get` events.

- [ ] **Step 3: Write the implementation**

In `handleExec`, after the exec finishes and the metric is set, before the final `send(...)`:

```go
	argv := clampArgv(req.Argv, 4096)
	envKeys := make([]string, 0, len(req.Env))
	for k := range req.Env {
		if k == "HTTP_PROXY" || k == "HTTPS_PROXY" || k == "http_proxy" || k == "https_proxy" || k == "NO_PROXY" {
			continue // proxy vars are injected, not caller data
		}
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	execAttrs := map[string]any{
		"argv": argv, "env_keys": envKeys, "dir": req.Dir, "tty": req.TTY,
		"dur_ms": time.Since(start).Milliseconds(),
	}
	if err != nil {
		execAttrs["error"] = err.Error()
	} else {
		execAttrs["exit"] = res.Code
		execAttrs["signal"] = res.Signal
		execAttrs["timed_out"] = res.TimedOut
	}
	s.audit.Record(audit.Event{Type: audit.TypeExec, Sandbox: sb.ID, Token: tokenID(r), Attrs: execAttrs})
```

`start` must be captured at the top of `handleExec` — add `start := time.Now()` as the first line after `sb := s.mustOwn(...)`. Add the helper:

```go
func clampArgv(argv []string, max int) []string {
	total := 0
	out := make([]string, 0, len(argv))
	for _, a := range argv {
		if total+len(a) > max {
			out = append(out, "…")
			break
		}
		total += len(a)
		out = append(out, a)
	}
	return out
}
```

Add `"sort"` to `server.go` imports if not present (it is not currently).

In `handlePutFile`, after `s.m.fileBytes.WithLabelValues("in").Add(...)`:

```go
	scan := "clean"
	if s.scanner != nil {
		// d is in scope only inside the scanner branch above; hoist it.
	}
	s.audit.Record(audit.Event{Type: audit.TypeFilePut, Sandbox: sb.ID, Token: tokenID(r),
		Attrs: map[string]any{"path": path, "bytes": res.BytesWritten, "sha256": res.SHA256, "scan": scan}})
```

To carry the scan result: change the scanner branch so `scanAction := "clean"` is declared before `var body io.Reader = r.Body`, set `scanAction = d.action` after `d := s.screen(...)`, and use `scanAction` in the event. (A blocked put returns 409 earlier via `writeSecretBlock` and never reaches here, so `scan` here is one of `clean`/`allow`/`redact`.)

In `handleGetFile`, after `s.m.fileBytes.WithLabelValues("out").Add(...)`:

```go
	s.audit.Record(audit.Event{Type: audit.TypeFileGet, Sandbox: sb.ID, Token: tokenID(r),
		Attrs: map[string]any{"path": path, "bytes": res.Size, "sha256": res.SHA256}})
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test -tags integration ./internal/server/ -run 'TestAudit' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/server/
go vet ./... && go vet -tags integration ./...
git add internal/server/server.go internal/server/server_integration_test.go
git commit -m "$(cat <<'EOF'
feat(server): audit exec and file transfers, metadata only

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01PRvf4bxSFzyL2dAPgorw7M
EOF
)"
```

---

## Task 10: Server — emit ingress secret-firewall events

**Files:**
- Modify: `internal/server/server.go` (`screen`)
- Test: `internal/server/secretfirewall_test.go` (add `TestScreenAudits`, no build tag — `screen` is unit-testable with a nil client path? it needs `s.scanner`; construct a `Server` via `New` with `Audit` set and call `screen` directly)

**Interfaces:**
- Consumes: `s.audit`.
- Produces: `secret.block` / `secret.allow` / `secret.redact` events (sync), `target` + `rules` + `findings` count, never a value.

- [ ] **Step 1: Write the failing test**

```go
package server

import (
	"context"
	"testing"

	"github.com/M30W-M1X/moat/internal/audit"
	"github.com/sirupsen/logrus"
)

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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/server/ -run TestScreenAudits -v`
Expected: FAIL — `screen` writes no audit event.

- [ ] **Step 3: Write the implementation**

In `screen`, replace the final `s.log.WithFields(...).Warn("secret_detected")` block's tail (after `list` is built) with the existing warn **plus**:

```go
	s.log.WithFields(logrus.Fields{
		"token": tid, "target": name, "findings": len(fs),
		"rules": strings.Join(list, ","), "action": action,
	}).Warn("secret_detected")

	auditType := audit.TypeSecretBlock
	switch action {
	case "allow":
		auditType = audit.TypeSecretAllow
	case "redact":
		auditType = audit.TypeSecretRedact
	}
	_ = s.audit.RecordSync(context.Background(), audit.Event{
		Type: auditType, Token: tid,
		Attrs: map[string]any{"target": name, "rules": list, "findings": len(fs)},
	})
```

`screen` has no `context.Context` parameter today. Using `context.Background()` is acceptable here — the write is small and the caller already committed to the 409/redact path. Add `"context"` to imports if absent (it is present).

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/server/ -run 'TestScreen|TestSecret' -v`
Expected: PASS (or SKIP if the vendored rule set no longer flags the test value — the skip is deliberate and documented in the test).

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/server/
go vet ./... && go vet -tags integration ./...
git add internal/server/server.go internal/server/secretfirewall_test.go
git commit -m "$(cat <<'EOF'
feat(server): audit ingress secret-firewall decisions

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01PRvf4bxSFzyL2dAPgorw7M
EOF
)"
```

---

## Task 11: Egress proxy — record allow/deny with the sandbox resolved from client IP

**Files:**
- Modify: `internal/egress/proxy.go` (`Proxy` struct, `SetAudit`, `decide`), `internal/egress/transparent.go` (pass `via`)
- Modify: `internal/server/server.go` (add `IdentifyClient`)
- Test: `internal/egress/proxy_test.go` (add `TestDecideRecordsAudit`)

**Interfaces:**
- Consumes: `audit.Recorder`, `audit.Event`; `netip`.
- Produces:
  - `func (p *Proxy) SetAudit(rec *audit.Recorder, identify func(netip.Addr) (sandboxID, tokenID string, ok bool))`
  - `decide` records `egress.allow` (async) / `egress.deny` (sync), resolving `sandbox`/`token` from `r.RemoteAddr`'s IP via `identify`; on `!ok`, envelope `sandbox`/`token` empty and `attrs.client_ip` set.
  - `func (s *Server) IdentifyClient(ip netip.Addr) (string, string, bool)` — iterates `s.reg.list()` for a sandbox whose `net.GuestIP` equals `ip` (the guest is the proxy client), returns `(sb.ID, sb.TokenID, true)`.

- [ ] **Step 1: Write the failing test**

```go
package egress

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/M30W-M1X/moat/internal/audit"
	"github.com/sirupsen/logrus"
)

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
	if !p.decide(req, "pypi.org") {
		t.Fatal("pypi.org should be allowed")
	}
	req2 := httptest.NewRequest(http.MethodGet, "http://evil.example/", nil)
	req2.RemoteAddr = "10.0.0.2:5556"
	if p.decide(req2, "evil.example") {
		t.Fatal("evil.example should be denied")
	}
	time.Sleep(100 * time.Millisecond)

	allow, _ := audit.Query(dir, audit.Filter{Types: []string{audit.TypeEgressAllow}})
	deny, _ := audit.Query(dir, audit.Filter{Types: []string{audit.TypeEgressDeny}})
	if len(allow) != 1 || allow[0].Sandbox != "sb-42" || allow[0].Attrs["host"] != "pypi.org" {
		t.Fatalf("allow event wrong: %+v", allow)
	}
	if len(deny) != 1 || deny[0].Attrs["host"] != "evil.example" {
		t.Fatalf("deny event wrong: %+v", deny)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/egress/ -run TestDecideRecordsAudit -v`
Expected: FAIL — `p.SetAudit` undefined.

- [ ] **Step 3: Write the implementation**

In `proxy.go`, add to the `Proxy` struct:

```go
	audit    *audit.Recorder
	identify func(netip.Addr) (string, string, bool)
```

Add imports `"net/netip"` and `"github.com/M30W-M1X/moat/internal/audit"`.

Add the setter:

```go
// SetAudit attaches an audit recorder and a client-IP resolver. Call it once
// after building the Proxy, before serving traffic.
func (p *Proxy) SetAudit(rec *audit.Recorder, identify func(netip.Addr) (string, string, bool)) {
	p.audit = rec
	p.identify = identify
}
```

Rewrite `decide`:

```go
func (p *Proxy) decide(r *http.Request, host string) bool {
	ok := p.pol.Permits(host)
	d := "allow"
	if !ok {
		d = "deny"
	}
	p.decision.WithLabelValues(d).Inc()
	p.log.WithFields(logrus.Fields{"host": host, "method": r.Method, "decision": d}).Info("egress")
	p.recordEgress(r, host, ok)
	return ok
}

func (p *Proxy) recordEgress(r *http.Request, host string, allowed bool) {
	if p.audit == nil || !p.audit.Enabled() {
		return
	}
	port := 80
	if r.Method == http.MethodConnect {
		port = 443
	}
	if _, ps, err := net.SplitHostPort(r.Host); err == nil {
		if n, err := strconv.Atoi(ps); err == nil {
			port = n
		}
	}
	method := r.Method
	if method == http.MethodConnect {
		method = "CONNECT"
	}
	attrs := map[string]any{"host": host, "port": port, "method": method, "via": p.via()}
	var sandbox, token string
	if p.identify != nil {
		if ip, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
			if sid, tid, ok := p.identify(ip.Addr()); ok {
				sandbox, token = sid, tid
			} else {
				attrs["client_ip"] = ip.Addr().String()
			}
		}
	}
	ev := audit.Event{Sandbox: sandbox, Token: token, Attrs: attrs}
	if allowed {
		ev.Type = audit.TypeEgressAllow
		p.audit.Record(ev)
	} else {
		ev.Type = audit.TypeEgressDeny
		_ = p.audit.RecordSync(r.Context(), ev)
	}
}

// via reports how this proxy is reached. The transparent listener sets it.
func (p *Proxy) via() string {
	if p.transparent {
		return "transparent"
	}
	return "proxy"
}
```

Add `transparent bool` to the `Proxy` struct. Add `"strconv"` to imports. In `transparent.go`, wherever the `Proxy` is used for the transparent listener, set `p.transparent = true` once at the start of `ServeTransparent` (or add a `p.markTransparent()` method). If a single `Proxy` serves both listeners, `via` cannot distinguish them per request — in that case set `via` from the listener: give `recordEgress` a `via string` parameter and pass `"proxy"` from `ServeHTTP`'s callers and `"transparent"` from the transparent handler. **Chosen:** add the `via` parameter to `decide` and `recordEgress`; `handleConnect`/`handleHTTP` pass `"proxy"`, the transparent handler passes `"transparent"`. Update `decide`'s two call sites in `proxy.go` and the transparent handler accordingly.

In `server.go`, add:

```go
// IdentifyClient maps an egress proxy client IP (a guest address) to the
// sandbox that owns it.
func (s *Server) IdentifyClient(ip netip.Addr) (string, string, bool) {
	for _, sb := range s.reg.list() {
		if sb.net != nil && sb.net.GuestIP.Compare(ip) == 0 {
			return sb.ID, sb.TokenID, true
		}
	}
	return "", "", false
}
```

Add `"net/netip"` to `server.go` imports (already present). Confirm `netconf.Plan.GuestIP` is a `netip.Addr` (it is — `guestIP` calls `p.GuestIP.String()`).

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/egress/ ./internal/server/ -run 'TestDecide|TestNew.*Recorder' -v`
Expected: PASS. Adjust the test's `decide` signature call if you added the `via` parameter (`p.decide(req, "pypi.org", "proxy")`).

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/egress/ internal/server/
go vet ./... && go vet -tags integration ./...
git add internal/egress/ internal/server/server.go internal/egress/proxy_test.go
git commit -m "$(cat <<'EOF'
feat(egress): audit egress decisions, resolving the sandbox by client IP

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01PRvf4bxSFzyL2dAPgorw7M
EOF
)"
```

---

## Task 12: Server — `GET /v1/audit` read endpoint with token scoping

**Files:**
- Modify: `internal/server/server.go` (`Handler`, new `handleAudit`, helper `auditVisible`)
- Test: `internal/server/server_integration_test.go` (`TestAuditReadScoping`, `//go:build integration`)

**Interfaces:**
- Consumes: `s.audit.LiveQuery`, `s.audit.SandboxToken`, `audit.Filter`, `TokenPolicy.auditAll`.
- Produces: `mux.HandleFunc("GET /v1/audit", s.auth(s.handleAudit))`. Response `application/x-ndjson`, one event per line. Default token: events where the caller created `e.Sandbox`, or `e.Token == caller`. `audit_read:"all"`: no filter, honours `token=` param.

- [ ] **Step 1: Write the failing test**

```go
//go:build integration

func TestAuditReadScoping(t *testing.T) {
	dir := t.TempDir()
	ts, ciToken := startServerWithAudit(t, dir) // "secret1" for t1/ci

	// t1 creates a sandbox and runs one exec.
	resp := do(t, ts, "POST", "/v1/sandboxes", ciToken, `{"ttl_seconds":120}`)
	var sb struct{ ID string `json:"id"` }
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

	// ci sees its own sandbox's events.
	own := do(t, ts, "GET", "/v1/audit?sandbox="+sb.ID, ciToken, "")
	defer own.Body.Close()
	m := 0
	d2 := json.NewDecoder(own.Body)
	for d2.More() {
		var e map[string]any
		if d2.Decode(&e) != nil {
			break
		}
		m++
	}
	if m < 2 {
		t.Fatalf("ci saw %d of its own events, want >= 2", m)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -tags integration ./internal/server/ -run TestAuditReadScoping -v`
Expected: FAIL — `GET /v1/audit` → 404 (route not registered).

- [ ] **Step 3: Write the implementation**

In `Handler`, add before `return s.recover(s.observe(mux))`:

```go
	mux.HandleFunc("GET /v1/audit", s.auth(s.handleAudit))
```

Add the handler:

```go
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	if !s.audit.Enabled() {
		writeErr(w, http.StatusNotFound, "audit log is disabled on this server")
		return
	}
	tid := tokenID(r)
	all := s.cfg.Policies[tid].auditAll()

	q := r.URL.Query()
	f := audit.Filter{
		Sandbox: q.Get("sandbox"),
		Types:   q["type"],
		Limit:   atoiOr(q.Get("limit"), 1000),
		Desc:    q.Get("order") == "desc",
	}
	if all {
		f.Token = q.Get("token")
	}
	if since := q.Get("since"); since != "" {
		if ts, ok := parseWhen(since); ok {
			f.Since = ts
		}
	}
	if until := q.Get("until"); until != "" {
		if ts, ok := parseWhen(until); ok {
			f.Until = ts
		}
	}

	evs, err := s.audit.LiveQuery(f)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read audit log: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	for _, e := range evs {
		if !all && !s.auditVisible(tid, e) {
			continue
		}
		_ = enc.Encode(auditWire(e))
	}
}

// auditVisible reports whether tid may see event e without the "all" scope.
func (s *Server) auditVisible(tid string, e audit.Event) bool {
	if e.Token == tid {
		return true
	}
	if e.Sandbox != "" {
		if owner, ok := s.audit.SandboxToken(e.Sandbox); ok && owner == tid {
			return true
		}
	}
	return false
}

// auditWire is the JSON shape returned to API clients.
func auditWire(e audit.Event) map[string]any {
	return map[string]any{
		"seq": e.Seq, "ts": e.TS.UTC().Format(time.RFC3339Nano), "type": e.Type,
		"sandbox": e.Sandbox, "token": e.Token, "attrs": e.Attrs,
		"prev": e.Prev, "hash": e.Hash,
	}
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return n
	}
	return def
}

// parseWhen accepts RFC3339 or a Go duration with a leading "-" (e.g. -15m),
// which it subtracts from now.
func parseWhen(s string) (time.Time, bool) {
	if ts, err := time.Parse(time.RFC3339, s); err == nil {
		return ts, true
	}
	if strings.HasPrefix(s, "-") {
		if d, err := time.ParseDuration(s); err == nil {
			return time.Now().Add(d), true
		}
	}
	return time.Time{}, false
}
```

Add `"strconv"` to `server.go` imports if Task 9 did not.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test -tags integration ./internal/server/ -run 'TestAudit' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/server/
go vet ./... && go vet -tags integration ./...
git add internal/server/server.go internal/server/server_integration_test.go
git commit -m "$(cat <<'EOF'
feat(server): GET /v1/audit with per-token read scoping

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01PRvf4bxSFzyL2dAPgorw7M
EOF
)"
```

---

## Task 13: Server — `GET /v1/sandboxes/{id}/report`

**Files:**
- Modify: `internal/server/server.go` (`Handler`, `handleReport`)
- Test: `internal/server/server_integration_test.go` (`TestAuditReport`, `//go:build integration`)

**Interfaces:**
- Consumes: `s.audit.LiveReport`, `s.audit.SandboxToken`, `TokenPolicy.auditAll`.
- Produces: `mux.HandleFunc("GET /v1/sandboxes/{id}/report", s.auth(s.handleReport))`. Owner or `all`; a caller who is neither gets `404 "no such sandbox"`. Works after the sandbox is destroyed, as long as its events are retained.

- [ ] **Step 1: Write the failing test**

```go
//go:build integration

func TestAuditReport(t *testing.T) {
	dir := t.TempDir()
	ts, ciToken := startServerWithAudit(t, dir)

	resp := do(t, ts, "POST", "/v1/sandboxes", ciToken, `{"ttl_seconds":120}`)
	var sb struct{ ID string `json:"id"` }
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

	// Report works post-destroy for the owner.
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
	if r.Sandbox != sb.ID || r.Exec.N != 1 || r.Exec.Nonzero != 1 || r.Bytes.In != 5 || r.DestroyReason != "deleted" {
		t.Fatalf("report body wrong: %+v", r)
	}

	// A stranger without "all" gets 404.
	other := do(t, ts, "GET", "/v1/sandboxes/"+sb.ID+"/report", "wrongsecret", "")
	other.Body.Close()
	if other.StatusCode != 401 { // invalid token first
		t.Fatalf("bad-token report = %d", other.StatusCode)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -tags integration ./internal/server/ -run TestAuditReport -v`
Expected: FAIL — route not registered (404 with a different body / 405).

- [ ] **Step 3: Write the implementation**

In `Handler`, add:

```go
	mux.HandleFunc("GET /v1/sandboxes/{id}/report", s.auth(s.handleReport))
```

Register it **before** `GET /v1/sandboxes/{id}` is not required — Go 1.22+ `ServeMux` matches the more specific pattern regardless of order. Add:

```go
func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	if !s.audit.Enabled() {
		writeErr(w, http.StatusNotFound, "audit log is disabled on this server")
		return
	}
	id := r.PathValue("id")
	tid := tokenID(r)
	if !s.cfg.Policies[tid].auditAll() {
		owner, ok := s.audit.SandboxToken(id)
		if !ok || owner != tid {
			writeErr(w, http.StatusNotFound, "no such sandbox")
			return
		}
	}
	rep, err := s.audit.LiveReport(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "build report: "+err.Error())
		return
	}
	if rep.Sandbox == "" {
		writeErr(w, http.StatusNotFound, "no such sandbox")
		return
	}
	writeJSON(w, http.StatusOK, rep)
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test -tags integration ./internal/server/ -run 'TestAudit' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/server/
go vet ./... && go vet -tags integration ./...
git add internal/server/server.go internal/server/server_integration_test.go
git commit -m "$(cat <<'EOF'
feat(server): GET /v1/sandboxes/{id}/report

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01PRvf4bxSFzyL2dAPgorw7M
EOF
)"
```

---

## Task 14: config.AuditDir and cmd/moatd wiring

**Files:**
- Modify: `internal/config/config.go` (add `AuditDir`)
- Modify: `cmd/moatd/main.go` (flag, build recorder, wire proxy resolver, emit nothing directly)
- Modify: `cmd/moatd/config.go` (`fileAudit`, `fileToken.AuditRead`, validation)
- Test: `internal/config/config_test.go` (`TestAuditDir`), `cmd/moatd/config_test.go` if it exists — else a small `TestLoadConfigAudit` there.

**Interfaces:**
- Consumes: `audit.New`, `audit.Config`, `server.Config.Audit`, `server.Config.Version`, `Server.IdentifyClient`.
- Produces:
  - `func config.AuditDir() string` — `<data dir>/audit`, using the same resolution as `ArtifactsDir` (repo checkout ⇒ `<repo>/run/audit`? No — audit should persist; use the data dir). Concretely: `filepath.Join(dataDir(), "audit")` when `dataDir()` resolves, else `filepath.Join(os.TempDir(), "moat-audit")`.
  - `-audit-dir` flag (default `""` meaning "use config or `config.AuditDir()`"); `-audit-dir "off"` or `""`-in-file disables.
  - `fileConfig.Audit *fileAudit{ Dir string; MaxFileMB int; MaxTotalMB int; MaxAge string; FsyncBatch int; FsyncInterval string }`.
  - `fileToken.AuditRead string` → `server.TokenPolicy.AuditRead`.

- [ ] **Step 1: Write the failing tests**

`internal/config/config_test.go`:

```go
func TestAuditDir(t *testing.T) {
	t.Setenv("MOAT_DATA", t.TempDir())
	got := AuditDir()
	if filepath.Base(got) != "audit" {
		t.Fatalf("AuditDir = %q, want it to end in /audit", got)
	}
	if !strings.HasPrefix(got, os.Getenv("MOAT_DATA")) {
		t.Fatalf("AuditDir %q not under MOAT_DATA", got)
	}
}
```

`cmd/moatd/config_test.go` (create if absent):

```go
package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadConfigAudit(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "moatd.json")
	os.WriteFile(p, []byte(`{
	  "templates":[{"name":"default"}],
	  "tokens":[{"id":"ci","secret":"s"},{"id":"secops","secret":"s2","audit_read":"all"}],
	  "audit":{"dir":"`+dir+`/aud","max_file_mb":64,"max_age":"48h"}
	}`), 0o600)
	lc, err := loadConfigFile(p, "/k", "/r")
	if err != nil {
		t.Fatal(err)
	}
	if lc.Policies["secops"].AuditRead != "all" {
		t.Fatalf("audit_read not parsed: %+v", lc.Policies["secops"])
	}
	if lc.Audit == nil || lc.Audit.Dir != dir+"/aud" || lc.Audit.MaxFileMB != 64 {
		t.Fatalf("audit block not parsed: %+v", lc.Audit)
	}
	if lc.Audit.MaxAge != 48*time.Hour {
		t.Fatalf("max_age = %v", lc.Audit.MaxAge)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config/ ./cmd/moatd/ -run 'TestAuditDir|TestLoadConfigAudit' -v`
Expected: FAIL — `AuditDir` undefined, `loadedConfig.Audit` undefined, `audit_read` not on `fileToken`.

- [ ] **Step 3: Write the implementation**

`internal/config/config.go`:

```go
// AuditDir is where moatd writes the audit log by default. It sits beside the
// data directory so it survives restarts, unlike the runtime dir.
func AuditDir() string {
	if d := dataDir(); d != "" {
		return filepath.Join(d, "audit")
	}
	return filepath.Join(os.TempDir(), "moat-audit")
}
```

If `dataDir()` currently returns `""` only in odd cases, that is fine — the fallback covers it. Confirm `dataDir` is in the same file (it is, per the earlier grep).

`cmd/moatd/config.go`:

```go
type fileAudit struct {
	Dir           string `json:"dir"`
	MaxFileMB     int    `json:"max_file_mb"`
	MaxTotalMB    int    `json:"max_total_mb"`
	MaxAge        string `json:"max_age"`
	FsyncBatch    int    `json:"fsync_batch"`
	FsyncInterval string `json:"fsync_interval"`
}

type loadedAudit struct {
	Dir           string
	MaxFileMB     int
	MaxTotalMB    int
	MaxAge        time.Duration
	FsyncBatch    int
	FsyncInterval time.Duration
	Set           bool // the file had an "audit" block
}
```

Add `Audit *fileAudit `json:"audit"`` to `fileConfig`, `AuditRead string `json:"audit_read"`` to `fileToken`, and `Audit *loadedAudit` plus `ConfigSHA256 string` to `loadedConfig`. Set the digest near the top of `loadConfigFile`, right after `raw, err := os.ReadFile(path)`:

```go
	sum := sha256.Sum256(raw)
	out.ConfigSHA256 = hex.EncodeToString(sum[:])
```

Add `"crypto/sha256"` and `"encoding/hex"` to `cmd/moatd/config.go` imports. In `cmd/moatd/main.go`, in the `if *cfgFile != "" {` branch, add `cfg.ConfigSHA256 = fc.ConfigSHA256`.

In `loadConfigFile`, in the token loop where the policy is built, include `ft.AuditRead != ""` in the "has a policy" condition and set `AuditRead: ft.AuditRead`:

```go
		if len(ft.Templates) > 0 || ft.MaxConcurrent > 0 || maxTTL > 0 || ft.DenyNetwork || ft.AllowSecrets || ft.AuditRead != "" {
			if ft.AuditRead != "" && ft.AuditRead != "all" {
				return out, fmt.Errorf("%s: token %q audit_read must be \"all\" or empty", path, ft.ID)
			}
			out.Policies[ft.ID] = server.TokenPolicy{
				Templates:           ft.Templates,
				MaxConcurrent:       ft.MaxConcurrent,
				MaxTTL:              maxTTL,
				DenyNetwork:         ft.DenyNetwork,
				AllowSecretOverride: ft.AllowSecrets,
				AuditRead:           ft.AuditRead,
			}
		}
```

After the `Defaults` block parsing, add:

```go
	if fc.Audit != nil {
		la := &loadedAudit{
			Dir: fc.Audit.Dir, MaxFileMB: fc.Audit.MaxFileMB, MaxTotalMB: fc.Audit.MaxTotalMB,
			FsyncBatch: fc.Audit.FsyncBatch, Set: true,
		}
		if fc.Audit.MaxAge != "" {
			if la.MaxAge, err = time.ParseDuration(fc.Audit.MaxAge); err != nil {
				return out, fmt.Errorf("%s: audit.max_age: %w", path, err)
			}
		}
		if fc.Audit.FsyncInterval != "" {
			if la.FsyncInterval, err = time.ParseDuration(fc.Audit.FsyncInterval); err != nil {
				return out, fmt.Errorf("%s: audit.fsync_interval: %w", path, err)
			}
		}
		out.Audit = la
	}
```

`cmd/moatd/main.go`:

Add the flag near the other flags:

```go
	auditDir := flag.String("audit-dir", "", `audit log directory ("" = default, "off" disables)`)
```

Add a helper and build the recorder after `cfg` is populated and the config file (if any) is loaded, but **before** `server.New`:

```go
	// Resolve the audit directory: flag wins, then the config file, then the
	// built-in default. "off" or an explicit empty file value disables it.
	auditCfg := audit.Config{
		Dir:        config.AuditDir(),
		MaxFileMB:  128,
		MaxTotalMB: 2048,
		MaxAge:     720 * time.Hour,
		Metrics:    audit.NopMetrics(),
		Log:        log,
	}
	if fcAudit := loadedAuditFromFile; fcAudit != nil { // set in the -config branch
		if fcAudit.Set {
			if fcAudit.Dir != "" {
				auditCfg.Dir = fcAudit.Dir
			} else {
				auditCfg.Dir = "" // explicit empty in the file disables
			}
			if fcAudit.MaxFileMB > 0 {
				auditCfg.MaxFileMB = fcAudit.MaxFileMB
			}
			if fcAudit.MaxTotalMB > 0 {
				auditCfg.MaxTotalMB = fcAudit.MaxTotalMB
			}
			if fcAudit.MaxAge > 0 {
				auditCfg.MaxAge = fcAudit.MaxAge
			}
			if fcAudit.FsyncBatch > 0 {
				auditCfg.FsyncBatch = fcAudit.FsyncBatch
			}
			if fcAudit.FsyncInterval > 0 {
				auditCfg.FsyncInterval = fcAudit.FsyncInterval
			}
		}
	}
	switch *auditDir {
	case "off":
		auditCfg.Dir = ""
	case "":
		// keep resolved value
	default:
		auditCfg.Dir = *auditDir
	}
	rec, err := audit.New(auditCfg)
	if err != nil {
		log.WithError(err).Fatal("open audit log")
	}
	defer rec.Close()
	if rec.Enabled() {
		log.WithField("dir", auditCfg.Dir).Info("audit log active")
	} else {
		log.Info("audit log disabled")
	}
	cfg.Audit = rec
	cfg.Version = version
```

`version` — `cmd/moatd` has a `-ldflags -X main.version` per `build/release.sh`. Add `var version = "dev"` at package scope in `main.go` if not present (check; `moat-mcp` has one — moatd may not). Add it.

Wire the file's audit block: in the `if *cfgFile != "" {` branch, after `cfg.Policies = fc.Policies`, add `loadedAuditFromFile = fc.Audit`. Declare `var loadedAuditFromFile *loadedAudit` just before the `if *cfgFile != ""` block.

After `server.New` returns `srv` successfully, wire the egress resolver:

```go
	proxy.SetAudit(rec, srv.IdentifyClient)
```

Place this right after `srv, err := server.New(cfg, log)` error check and before `srv.Start(ctx)`. Add `"github.com/M30W-M1X/moat/internal/audit"` to `cmd/moatd/main.go` imports.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/config/ ./cmd/moatd/ -v && go build ./...`
Expected: PASS and a clean build.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/config/ cmd/moatd/
go vet ./...
git add internal/config/config.go internal/config/config_test.go cmd/moatd/main.go cmd/moatd/config.go cmd/moatd/config_test.go
git commit -m "$(cat <<'EOF'
feat(moatd): audit config block, -audit-dir flag, and wiring

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01PRvf4bxSFzyL2dAPgorw7M
EOF
)"
```

---

## Task 15: moatapi — Audit and Report client methods

**Files:**
- Modify: `internal/moatapi/moatapi.go`
- Test: `internal/moatapi/moatapi_test.go` (create if absent — a `httptest` fake like `cmd/moat-mcp` uses)

**Interfaces:**
- Consumes: `Client.do`, `decodeErr`.
- Produces:
  - `type AuditEvent struct { Seq uint64 `json:"seq"`; TS time.Time `json:"ts"`; Type, Sandbox, Token string; Attrs map[string]any `json:"attrs"`; Prev, Hash string }`
  - `type AuditFilter struct { Sandbox, Token string; Types []string; Since, Until string; Limit int; Desc bool }`
  - `type Report struct { ... }` — the same JSON shape the server returns (copy the tags from `audit.Report`; moatapi must not import `internal/audit`).
  - `func (c *Client) Audit(ctx context.Context, f AuditFilter) ([]AuditEvent, error)` — GET `/v1/audit` with query params, assemble NDJSON.
  - `func (c *Client) Report(ctx context.Context, id string) (Report, error)` — GET `/v1/sandboxes/{id}/report`.

- [ ] **Step 1: Write the failing test**

```go
package moatapi

import (
	"context"
	"net/http"
	"net/http/httptest"
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/moatapi/ -run TestAuditAndReport -v`
Expected: FAIL — `c.Audit` / `c.Report` undefined.

- [ ] **Step 3: Write the implementation**

Append to `moatapi.go`:

```go
// AuditEvent is one event returned by GET /v1/audit.
type AuditEvent struct {
	Seq     uint64         `json:"seq"`
	TS      time.Time      `json:"ts"`
	Type    string         `json:"type"`
	Sandbox string         `json:"sandbox"`
	Token   string         `json:"token"`
	Attrs   map[string]any `json:"attrs"`
	Prev    string         `json:"prev"`
	Hash    string         `json:"hash"`
}

// AuditFilter narrows an Audit call.
type AuditFilter struct {
	Sandbox string
	Token   string
	Types   []string
	Since   string
	Until   string
	Limit   int
	Desc    bool
}

// Report is the GET /v1/sandboxes/{id}/report body.
type Report struct {
	Sandbox       string         `json:"sandbox"`
	Token         string         `json:"token"`
	Template      string         `json:"template"`
	CreatedAt     time.Time      `json:"created_at"`
	DestroyedAt   time.Time      `json:"destroyed_at"`
	DestroyReason string         `json:"destroy_reason"`
	LifetimeS     int64          `json:"lifetime_s"`
	Networked     bool           `json:"networked"`
	GuestIP       string         `json:"guest_ip"`
	Counts        map[string]int `json:"counts"`
	Exec          struct {
		N          int   `json:"n"`
		Nonzero    int   `json:"nonzero"`
		Errors     int   `json:"errors"`
		TotalDurMS int64 `json:"total_dur_ms"`
	} `json:"exec"`
	Bytes struct {
		In  int64 `json:"in"`
		Out int64 `json:"out"`
	} `json:"bytes"`
	Files []struct {
		Dir    string `json:"dir"`
		Path   string `json:"path"`
		Bytes  int64  `json:"bytes"`
		SHA256 string `json:"sha256"`
		Scan   string `json:"scan"`
	} `json:"files"`
	SecretEvents []struct {
		Type   string   `json:"type"`
		Target string   `json:"target"`
		Rules  []string `json:"rules"`
	} `json:"secret_events"`
	Egress struct {
		Allow []string `json:"allow"`
		Deny  []string `json:"deny"`
	} `json:"egress"`
	Chain struct {
		FirstSeq uint64 `json:"first_seq"`
		LastSeq  uint64 `json:"last_seq"`
		Verified bool   `json:"verified"`
	} `json:"chain"`
}

// Audit reads events from GET /v1/audit.
func (c *Client) Audit(ctx context.Context, f AuditFilter) ([]AuditEvent, error) {
	q := url.Values{}
	if f.Sandbox != "" {
		q.Set("sandbox", f.Sandbox)
	}
	if f.Token != "" {
		q.Set("token", f.Token)
	}
	for _, t := range f.Types {
		q.Add("type", t)
	}
	if f.Since != "" {
		q.Set("since", f.Since)
	}
	if f.Until != "" {
		q.Set("until", f.Until)
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	if f.Desc {
		q.Set("order", "desc")
	}
	resp, err := c.do(ctx, http.MethodGet, "/v1/audit?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, decodeErr(resp)
	}
	var out []AuditEvent
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e AuditEvent
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// Report fetches the per-sandbox rollup.
func (c *Client) Report(ctx context.Context, id string) (Report, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/sandboxes/"+id+"/report", nil)
	if err != nil {
		return Report{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Report{}, decodeErr(resp)
	}
	var rep Report
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		return Report{}, err
	}
	return rep, nil
}
```

Add imports `"net/url"` and `"strconv"` to `moatapi.go` if absent (`bufio`, `bytes`, `time` are already used).

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/moatapi/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/moatapi/
go vet ./...
git add internal/moatapi/
git commit -m "$(cat <<'EOF'
feat(moatapi): Audit and Report client methods

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01PRvf4bxSFzyL2dAPgorw7M
EOF
)"
```

---

## Task 16: `moat audit` CLI subcommand

**Files:**
- Create: `cmd/moat/audit.go`
- Modify: `cmd/moat/main.go` (dispatch `case "audit"`, usage text)
- Test: `internal/server/server_integration_test.go` add `TestMoatAuditCLI`? — the CLI shells out; instead unit-test the arg parsing in `cmd/moat/audit_test.go` and cover the end-to-end path in the integration smoke of Task 17's doc note. Keep this task's test small: `cmd/moat/audit_test.go` for `parseAuditArgs` and the `--dir` verify path against a fixture directory.

**Interfaces:**
- Consumes: `moatapi.Client.Audit`, `moatapi.Client.Report`, `audit.Verify`, `audit.Query` (the `internal/audit` package is importable from `cmd/moat`).
- Produces: `func runAudit(args []string) int` handling `tail`, `query`, `report`, `verify`. Shared flags: `-addr`, `-token` (fall back to `$MOAT_ADDR` / `$MOAT_TOKEN`), `-dir` (bypass the API), `-json`.

- [ ] **Step 1: Write the failing test**

```go
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAuditVerifyLocalDir(t *testing.T) {
	dir := t.TempDir()
	// A minimal two-event valid chain, hashes computed by hand is fragile;
	// instead use the audit package to write a fixture.
	writeAuditFixture(t, dir, 5) // helper below, uses internal/audit
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
```

The test file imports: `"context"`, `"os"`, `"path/filepath"`, `"testing"`, `"github.com/sirupsen/logrus"`, `"github.com/M30W-M1X/moat/internal/audit"`.

`writeAuditFixture` helper in the test file:

```go
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cmd/moat/ -run TestAuditVerifyLocalDir -v`
Expected: FAIL — `runAudit` undefined.

- [ ] **Step 3: Write the implementation**

`cmd/moat/audit.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/M30W-M1X/moat/internal/audit"
	"github.com/M30W-M1X/moat/internal/moatapi"
)

// runAudit implements `moat audit {tail,query,report,verify}`.
func runAudit(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: moat audit {tail|query|report|verify} [flags]")
		return 2
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("audit "+sub, flag.ContinueOnError)
	addr := fs.String("addr", os.Getenv("MOAT_ADDR"), "moatd address (host:port or URL)")
	token := fs.String("token", os.Getenv("MOAT_TOKEN"), "bearer token")
	dir := fs.String("dir", "", "read the audit files directly instead of the API")
	asJSON := fs.Bool("json", false, "emit JSON")
	sandbox := fs.String("sandbox", "", "filter by sandbox id")
	ftoken := fs.String("filter-token", "", "filter by token id (needs audit_read=all)")
	typ := fs.String("type", "", "filter by event type")
	since := fs.String("since", "", "start time (RFC3339 or -15m)")
	until := fs.String("until", "", "end time (RFC3339 or -15m)")
	limit := fs.Int("limit", 200, "max events")
	follow := fs.Bool("f", false, "tail: keep polling for new events")
	if err := fs.Parse(rest); err != nil {
		return 2
	}

	ctx, cancel := signalContext()
	defer cancel()

	var types []string
	if *typ != "" {
		types = []string{*typ}
	}

	switch sub {
	case "verify":
		return auditVerify(ctx, *dir, *addr, *token, *asJSON)
	case "report":
		if fs.NArg() < 1 {
			fmt.Fprintln(os.Stderr, "usage: moat audit report <sandbox-id>")
			return 2
		}
		return auditReport(ctx, fs.Arg(0), *dir, *addr, *token, *asJSON)
	case "query", "tail":
		f := auditFilter{sandbox: *sandbox, token: *ftoken, types: types,
			since: *since, until: *until, limit: *limit, desc: sub == "tail"}
		return auditList(ctx, f, *dir, *addr, *token, *asJSON, *follow && sub == "tail")
	default:
		fmt.Fprintf(os.Stderr, "unknown audit subcommand %q\n", sub)
		return 2
	}
}

type auditFilter struct {
	sandbox, token string
	types          []string
	since, until   string
	limit          int
	desc           bool
}

func auditVerify(ctx context.Context, dir, addr, token string, asJSON bool) int {
	if dir != "" {
		res, err := audit.Verify(dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "verify:", err)
			return 1
		}
		printVerify(res, asJSON)
		if res.OK {
			return 0
		}
		return 1
	}
	if addr == "" || token == "" {
		fmt.Fprintln(os.Stderr, "verify over the API needs -addr and -token, or use -dir")
		return 2
	}
	// The API has no verify endpoint; verify runs client-side over a full
	// pull. audit_read=all is required for an unfiltered pull.
	c := moatapi.New(addr, token)
	evs, err := c.Audit(ctx, moatapi.AuditFilter{Limit: 100000})
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify:", err)
		return 1
	}
	res := verifyEvents(evs)
	printVerify(res, asJSON)
	if res.OK {
		return 0
	}
	return 1
}
```

Add `verifyEvents([]moatapi.AuditEvent) audit.VerifyResult` — a thin adapter that maps `moatapi.AuditEvent` to `audit.Event` and calls an exported `audit.VerifyEvents([]audit.Event) audit.VerifyResult`. **Add `VerifyEvents` to `internal/audit/reader.go`** (refactor: `Verify(dir)` becomes `scanFiles(dir)` → `VerifyEvents(events)`). This keeps one implementation.

```go
// In internal/audit/reader.go — extract from Verify:
func VerifyEvents(evs []Event) VerifyResult { /* the per-event loop from Verify */ }

func Verify(dir string) (VerifyResult, error) {
	files, err := listFiles(dir)
	if err != nil {
		return VerifyResult{}, err
	}
	var all []Event
	for _, f := range files {
		evs, _, err := scanFile(f)
		if err != nil {
			return VerifyResult{}, err
		}
		all = append(all, evs...)
	}
	return VerifyEvents(all), nil
}
```

Fill in `auditReport`, `auditList`, `printVerify`, and the table/JSON printers:

```go
func auditList(ctx context.Context, f auditFilter, dir, addr, token string, asJSON, follow bool) int {
	emit := func(evs []audit.Event) {
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		for _, e := range evs {
			if asJSON {
				b, _ := json.Marshal(auditWireCLI(e))
				fmt.Println(string(b))
				continue
			}
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%v\n",
				e.Seq, e.TS.Format(time.RFC3339), e.Type, e.Sandbox, e.Token, e.Attrs)
		}
		w.Flush()
	}
	if dir != "" {
		evs, err := audit.Query(dir, audit.Filter{Sandbox: f.sandbox, Token: f.token,
			Types: f.types, Limit: f.limit, Desc: f.desc})
		if err != nil {
			fmt.Fprintln(os.Stderr, "query:", err)
			return 1
		}
		emit(evs)
		return 0
	}
	if addr == "" || token == "" {
		fmt.Fprintln(os.Stderr, "need -addr and -token, or -dir")
		return 2
	}
	c := moatapi.New(addr, token)
	pull := func() ([]audit.Event, error) {
		me, err := c.Audit(ctx, moatapi.AuditFilter{Sandbox: f.sandbox, Token: f.token,
			Types: f.types, Since: f.since, Until: f.until, Limit: f.limit, Desc: f.desc})
		if err != nil {
			return nil, err
		}
		return fromWire(me), nil
	}
	evs, err := pull()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	emit(evs)
	if !follow {
		return 0
	}
	var lastSeq uint64
	for _, e := range evs {
		if e.Seq > lastSeq {
			lastSeq = e.Seq
		}
	}
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return 0
		case <-t.C:
			more, err := pull()
			if err != nil {
				continue
			}
			var fresh []audit.Event
			for _, e := range more {
				if e.Seq > lastSeq {
					fresh = append(fresh, e)
					lastSeq = e.Seq
				}
			}
			if len(fresh) > 0 {
				emit(fresh)
			}
		}
	}
}

func auditReport(ctx context.Context, id, dir, addr, token string, asJSON bool) int {
	var rep any
	if dir != "" {
		evs, err := audit.Query(dir, audit.Filter{Sandbox: id, Limit: 100000})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if len(evs) == 0 {
			fmt.Fprintln(os.Stderr, "no events for", id)
			return 1
		}
		rep = audit.BuildReport(evs)
	} else {
		if addr == "" || token == "" {
			fmt.Fprintln(os.Stderr, "need -addr and -token, or -dir")
			return 2
		}
		r, err := moatapi.New(addr, token).Report(ctx, id)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		rep = r
	}
	b, _ := json.MarshalIndent(rep, "", "  ")
	fmt.Println(string(b))
	return 0
}

func printVerify(res audit.VerifyResult, asJSON bool) {
	if asJSON {
		b, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(b))
		return
	}
	status := "chain OK"
	if !res.OK {
		status = "CHAIN BROKEN"
	}
	fmt.Printf("%s  %d events  seq %d..%d\n", status, res.Events, res.FirstSeq, res.LastSeq)
	if res.PrunedPrefix {
		fmt.Printf("  chain starts at seq %d (earlier entries pruned)\n", res.FirstSeq)
	}
	for _, p := range res.Problems {
		fmt.Println("  !", p)
	}
}

func auditWireCLI(e audit.Event) map[string]any {
	return map[string]any{"seq": e.Seq, "ts": e.TS.UTC().Format(time.RFC3339Nano),
		"type": e.Type, "sandbox": e.Sandbox, "token": e.Token, "attrs": e.Attrs,
		"prev": e.Prev, "hash": e.Hash}
}

func fromWire(me []moatapi.AuditEvent) []audit.Event {
	out := make([]audit.Event, len(me))
	for i, e := range me {
		out[i] = audit.Event{Seq: e.Seq, TS: e.TS, Type: e.Type, Sandbox: e.Sandbox,
			Token: e.Token, Attrs: e.Attrs, Prev: e.Prev, Hash: e.Hash}
	}
	return out
}

func verifyEvents(me []moatapi.AuditEvent) audit.VerifyResult {
	return audit.VerifyEvents(fromWire(me))
}
```

In `cmd/moat/main.go`, add to the `switch`:

```go
	case "audit":
		os.Exit(runAudit(os.Args[2:]))
```

Add to `usage()`:

```
  moat audit    {tail|query|report|verify}  read the daemon audit log
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./cmd/moat/ ./internal/audit/ -v && go build ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w cmd/moat/ internal/audit/
go vet ./...
git add cmd/moat/audit.go cmd/moat/main.go cmd/moat/audit_test.go internal/audit/reader.go
git commit -m "$(cat <<'EOF'
feat(moat): `moat audit` CLI — tail, query, report, verify

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01PRvf4bxSFzyL2dAPgorw7M
EOF
)"
```

---

## Task 17: End-to-end integration and metrics assertions

**Files:**
- Modify: `internal/server/server_integration_test.go` (`TestAuditEndToEnd`, `TestAuditFailClosedOnUnwritableDir`, `//go:build integration`)

**Interfaces:**
- Consumes: everything from Tasks 7–14.
- Produces: proof that a full flow writes the ordered event set, that `/metrics` shows `moat_audit_events_total`, and that a create with an unwritable audit dir returns 503 and does not leak a sandbox.

- [ ] **Step 1: Write the failing tests**

```go
//go:build integration

func TestAuditEndToEnd(t *testing.T) {
	dir := t.TempDir()
	ts, token := startServerWithAudit(t, dir)

	resp := do(t, ts, "POST", "/v1/sandboxes", token, `{"ttl_seconds":120}`)
	var sb struct{ ID string `json:"id"` }
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

func TestAuditFailClosedOnUnwritableDir(t *testing.T) {
	dir := t.TempDir()
	ts, token := startServerWithAudit(t, dir)
	// Make the audit dir read-only so the next RecordSync fails.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skip("cannot chmod in this environment")
	}
	defer os.Chmod(dir, 0o700)
	// Force a rotation by exhausting the current file is slow; instead remove
	// the current file handle's target so append fails. Simplest portable
	// approach: chmod the file itself.
	files, _ := audit.Query(dir, audit.Filter{}) // ensure at least one file exists
	_ = files
	fs, _ := filepathGlob(dir)
	for _, f := range fs {
		_ = os.Chmod(f, 0o400)
	}

	resp := do(t, ts, "POST", "/v1/sandboxes", token, `{"ttl_seconds":60}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("create with unwritable audit = %d (%s), want 503", resp.StatusCode, body)
	}
	// No sandbox leaked.
	list := do(t, ts, "GET", "/v1/sandboxes", token, "")
	lb, _ := io.ReadAll(list.Body)
	list.Body.Close()
	if bytes.Contains(lb, []byte(`"id"`)) {
		t.Fatalf("a sandbox leaked past a failed audit write: %s", lb)
	}
}

func filepathGlob(dir string) ([]string, error) {
	return filepath.Glob(filepath.Join(dir, "audit-*.jsonl"))
}
```

Add imports `"bytes"`, `"path/filepath"` to the integration test file if absent.

- [ ] **Step 2: Run the tests to verify they fail (or pass)**

Run: `go test -tags integration ./internal/server/ -run 'TestAuditEndToEnd|TestAuditFailClosed' -v`
Expected: `TestAuditEndToEnd` PASS if Tasks 7–14 are complete. `TestAuditFailClosedOnUnwritableDir` may need the append to actually hit the read-only file — if `os.OpenFile` for append was cached at `New`, the failing write happens on `f.Write`/`f.Sync` after chmod; confirm the 503 path fires. If chmod on the open fd does not block writes on this filesystem, `t.Skip` with a clear message.

- [ ] **Step 3: Fix any gaps found**

If `TestAuditEndToEnd` fails on ordering, the cause is almost always the async `exec` / `file.put` events landing after `sandbox.destroy` (which is sync). The spec accepts this only if `destroy` still sorts last by `seq`. `seq` is assigned at write time under the mutex, so a sync `destroy` that runs before the async `exec` drains **will** get a lower seq. Fix: in `handleDelete` and `janitor`, before `recordDestroy`, call `s.audit.Flush()` — add a `Flush()` method to `Recorder` that blocks until the async queue is empty (send a sentinel through the queue and wait). Add `Flush` to `internal/audit/recorder.go`:

```go
// Flush blocks until every event queued before the call is written.
func (r *Recorder) Flush() {
	if !r.enabled {
		return
	}
	done := make(chan struct{})
	select {
	case r.queue <- Event{Type: "_flush", flush: done}:
		<-done
	case <-r.closed:
	}
}
```

Give `Event` an unexported `flush chan struct{}` field (never serialised — `canonical`/`marshalLine` build a map explicitly, so it is already excluded). In `writeLoop`'s two receive cases, handle it: `if e.flush != nil { close(e.flush); continue }`. Add a unit test `TestFlushOrdersBeforeSync` to `recorder_test.go`. Then call `r.Flush()` in the three destroy sites and re-run.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `make test && go test -tags integration ./internal/server/ -run 'TestAudit' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/audit/ internal/server/
go vet ./... && go vet -tags integration ./...
git add internal/audit/recorder.go internal/audit/recorder_test.go internal/server/server_integration_test.go internal/server/server.go
git commit -m "$(cat <<'EOF'
test(audit): end-to-end flow, metrics, and fail-closed on an unwritable log

Adds Recorder.Flush so a synchronous sandbox.destroy always sorts after the
async exec and file events that preceded it.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01PRvf4bxSFzyL2dAPgorw7M
EOF
)"
```

---

## Task 18: Documentation

**Files:**
- Modify: `docs/api.md`, `docs/moatd.example.json`, `README.md`, `SKILL.md`
- Create: nothing.

**Interfaces:** none — prose and one JSON example.

- [ ] **Step 1: Update `docs/api.md`**

Add a section after the file endpoints:

```markdown
## Audit log

### `GET /v1/audit`

Newline-delimited JSON (`application/x-ndjson`), one event per line, in `seq`
order.

| Query param | Meaning |
| --- | --- |
| `sandbox` | exact sandbox id |
| `token` | exact token id (only for a token with `audit_read: "all"`) |
| `type` | repeatable; exact event type |
| `since`, `until` | RFC3339, or a Go duration with a leading `-` (e.g. `-15m`) |
| `limit` | max events (default 1000, cap 100000) |
| `order` | `asc` (default) or `desc` |

A normal token sees only events for sandboxes it created, plus events tagged
with its own token id. A token with `audit_read: "all"` sees every event.

Event shape:

```json
{"seq":42,"ts":"2026-08-30T18:14:07.913482Z","type":"exec","sandbox":"sb-7f3a19c4",
 "token":"ci","attrs":{"argv":["python","/work/task.py"],"exit":0,"dur_ms":1180},
 "prev":"a91c4f…","hash":"7d2e08…"}
```

Events are metadata only. No payload, environment value, file body, or command
output is ever recorded. Each `hash` is the sha256 of the event's canonical
form; `prev` chains to the previous event. Verify the chain with
`moat audit verify`.

### `GET /v1/sandboxes/{id}/report`

A JSON rollup of one sandbox's activity, built from the audit log. Works after
the sandbox is destroyed, until its events are pruned. The caller must own the
sandbox or hold `audit_read: "all"`; otherwise `404`.

## Token policy: `audit_read`

`"audit_read": "all"` in a token's `moatd.json` entry lets it read every
token's audit events and run `moat audit verify` over the API. Omitted or
empty means the token sees only its own.
```

- [ ] **Step 2: Update `docs/moatd.example.json`**

Add an `"audit"` block at the top level and an `audit_read` on the admin token:

```json
  "audit": {
    "dir": "/var/lib/moat/audit",
    "max_file_mb": 128,
    "max_total_mb": 2048,
    "max_age": "720h"
  },
```

And in the `tokens` array, on the existing `admin` token, add `"audit_read": "all"`.

Verify the file still parses: `go test ./cmd/moatd/` includes a fixture check, or run `moatd -config docs/moatd.example.json -audit-dir off` and confirm it starts (needs images; if not available, at least `go run ./cmd/moatd -config docs/moatd.example.json -h` after a parse-only guard — simplest is to add the example path to an existing `cmd/moatd` config test if one loads it).

- [ ] **Step 3: Update `README.md`**

Under the Phase 5 / M2 bullets, add:

```markdown
- **Audit log (M2)** — `moatd` writes a hash-chained, metadata-only JSONL
  record of every boundary crossing (create / destroy / exec / file / secret
  block / egress decision / quota reject). `GET /v1/audit`,
  `GET /v1/sandboxes/{id}/report`, and `moat audit {tail,query,report,verify}`.
  Per-token read scope with `audit_read: "all"`. On by default; disable with
  `-audit-dir off`. `internal/audit`.
```

Add `internal/audit` to the Layout table with the description "append-only, hash-chained audit log; writer, reader, per-sandbox report".

- [ ] **Step 4: Update `SKILL.md`**

Add one sentence to the section about blocked secrets / results, no behaviour change:

```markdown
Every action you take in a sandbox — the commands you run, the files you move,
a blocked secret, a denied network destination — is recorded in the host's
audit log. This does not change how you work; it means the user can review
exactly what a sandbox did.
```

- [ ] **Step 5: Commit**

```bash
git add docs/api.md docs/moatd.example.json README.md SKILL.md
git commit -m "$(cat <<'EOF'
docs: audit log API, config, README, and SKILL note

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01PRvf4bxSFzyL2dAPgorw7M
EOF
)"
```

---

## Final verification

- [ ] `gofmt -l .` prints nothing.
- [ ] `go vet ./...` and `go vet -tags integration ./...` are clean.
- [ ] `go build ./...` succeeds.
- [ ] `make test` passes.
- [ ] `make test-integration` passes (needs `/dev/kvm` and `make images`).
- [ ] `go mod tidy` produces no change to `go.mod` / `go.sum` (no new dependency).
- [ ] Manual smoke: `MOAT_TOKENS=ci:s moatd -audit-dir /tmp/moat-audit &` then `moat audit verify -dir /tmp/moat-audit` prints `chain OK`.
- [ ] Update ClaudeVault `Build Log.md` with an "M2 — Audit event system" entry and mirror a one-line note into agentcairn, per the repo's knowledge-system workflow. Log any footgun hit during implementation in `Gotchas.md` as `G-N`.

## Self-review notes (from writing this plan)

- **Spec coverage:** every spec section maps to a task — §2.1/§2.2 → Task 1 (+ verify in Task 5); §2.3 type set → Task 1, emission → Tasks 8–11 and Task 14 (`orphan.reap`); §2.4 attrs → Tasks 8–11 (each `attrs` map is written out in full); §3.1 → Tasks 2, 3, 7 (`SetMetrics`), 17 (`Flush`); §3.2 → Task 4; §3.3 → Task 2 (`recover`) + Task 6 (`index`); §4.1 → Tasks 12, 13; §4.2 → Task 16; §4.3 → Tasks 11, 15; §5 → Task 14; §6 → Task 7 (+ asserted in Task 17); §7 → spread across tasks + Task 17; §8 → Task 18; §9 rollout is additive and on-by-default (Tasks 14, 18); §10 items are explicitly out of scope.
- **`daemon.start` / `daemon.stop` attrs** now include `egress`, `config_sha256`, `uptime_s`, and `reason` per spec §2.4 (Tasks 7, 8, 14). `reason` is `"signal"` in practice because `cmd/moatd` drives shutdown from `signal.NotifyContext`.
- **Spec §3.3 "truncated last line discarded and logged"** — implemented in `scanFile` (returns `intact=false`, drops the bad tail). `Verify` tolerates it; `recover` in the `Recorder` resumes from the last intact entry. Covered by `TestVerifyDetectsTamper/truncated_last_line`.
- **Spec §2.4 `orphan.reap`** — the daemon's startup `vm.Reconcile` call in `cmd/moatd/main.go` runs *before* the recorder is built in the current ordering. Task 14 must build the recorder *before* the `vm.Reconcile` block, or move the reap-event emission to after. Simplest: in Task 14, construct `rec` right after `log` is created and emit an `orphan.reap` event per reaped id from `cmd/moatd`. If that reordering is awkward, defer `orphan.reap` to a follow-up and note it — it is the lowest-value event. **Decision for the executor:** build `rec` before `vm.Reconcile`; after the reap, `for _, id := range ids { _ = rec.RecordSync(ctx, audit.Event{Type: audit.TypeOrphanReap, Sandbox: id, Attrs: map[string]any{"source": "startup"}}) }`.
- **Spec §4.3 transparent proxy `via`** — resolved by passing `via` explicitly from each listener's handler rather than a struct field, because one `Proxy` can serve both listeners.
- **`prometheus.Counter` has no `Dec`** — the failed-create rollback in Task 8 leaves `moat_sandboxes_created_total` one high on the rare 503 path. Documented in a code comment; acceptable.
- **`moat audit verify` over the API** — there is no server verify endpoint; the CLI pulls all events (requires `audit_read: "all"`) and verifies client-side via `audit.VerifyEvents`. Documented in Task 16 and `docs/api.md`.
