package audit

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

// TestAppendLockedRemovesPartialWrite checks that a failed write leaves no
// partial line on disk. A test WriteFunc writes a few bytes then errors, like
// an ENOSPC write. appendLocked must roll the seq back and truncate the file
// to its pre-write size so the chain does not fork.
func TestAppendLockedRemovesPartialWrite(t *testing.T) {
	dir := t.TempDir()
	log := logrus.New()
	log.SetLevel(logrus.FatalLevel)

	var failNext atomic.Bool
	cfg := Config{
		Dir: dir, MaxFileMB: 8, MaxTotalMB: 64, MaxAge: time.Hour,
		FsyncBatch: 8, FsyncInterval: 20 * time.Millisecond,
		Metrics: NopMetrics(), Log: log,
		WriteFunc: func(f *os.File, b []byte) (int, error) {
			if failNext.CompareAndSwap(true, false) {
				n, _ := f.Write(b[:5]) // a few bytes land
				return n, errors.New("simulated write failure")
			}
			return f.Write(b)
		},
	}
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := r.RecordSync(ctx, Event{Type: TypeExec, Sandbox: "sb", Token: "ci"}); err != nil {
			t.Fatal(err)
		}
	}

	fileSize := func() int64 {
		files, _ := listFiles(dir)
		var total int64
		for _, f := range files {
			fi, _ := os.Stat(f)
			total += fi.Size()
		}
		return total
	}
	sizeBefore := fileSize()
	seqBefore := r.seq

	failNext.Store(true)
	if err := r.RecordSync(ctx, Event{Type: TypeExec, Sandbox: "sb", Token: "ci"}); err == nil {
		t.Fatal("RecordSync must return the write error")
	}
	if r.seq != seqBefore {
		t.Fatalf("seq = %d after a failed write, want %d", r.seq, seqBefore)
	}
	if got := fileSize(); got != sizeBefore {
		t.Fatalf("file size = %d after a failed write, want %d (partial bytes not removed)", got, sizeBefore)
	}

	// A following write still chains cleanly.
	if err := r.RecordSync(ctx, Event{Type: TypeExec, Sandbox: "sb", Token: "ci"}); err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	res, err := Verify(dir)
	if err != nil || !res.OK {
		t.Fatalf("chain broken after a partial write: %+v (%v)", res, err)
	}
	if res.Events != 4 {
		t.Fatalf("want 4 intact events, got %d", res.Events)
	}
}

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

func TestCloseIsIdempotent(t *testing.T) {
	r, _ := testRecorder(t)
	if err := r.RecordSync(context.Background(), Event{Type: TypeDaemonStart}); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("first Close returned %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("second Close returned %v", err)
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

func TestFlushOrdersBeforeSync(t *testing.T) {
	r, dir := testRecorder(t)
	for i := 0; i < 12; i++ {
		r.Record(Event{Type: TypeExec, Sandbox: "sb-f", Token: "ci"})
	}
	r.Flush()
	if err := r.RecordSync(context.Background(), Event{Type: TypeSandboxDestroy, Sandbox: "sb-f", Token: "ci"}); err != nil {
		t.Fatal(err)
	}
	_ = r.Close()

	evs := readLines(t, dir)
	var maxAsync, syncSeq uint64
	nAsync := 0
	for _, e := range evs {
		switch e.Type {
		case TypeExec:
			if e.Seq > maxAsync {
				maxAsync = e.Seq
			}
			nAsync++
		case TypeSandboxDestroy:
			syncSeq = e.Seq
		}
	}
	if nAsync != 12 {
		t.Fatalf("wrote %d async events, want 12", nAsync)
	}
	if syncSeq == 0 {
		t.Fatal("the sync event is not on disk")
	}
	if syncSeq <= maxAsync {
		t.Fatalf("sync seq %d is not after every async seq (max %d): Flush did not order the queue", syncSeq, maxAsync)
	}
}

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
