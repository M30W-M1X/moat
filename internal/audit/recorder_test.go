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
