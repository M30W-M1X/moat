package audit

import (
	"context"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

// TestSandboxTokenLookup checks the O(1) owner lookup. SandboxToken reads the
// in-memory index. It takes no file scan, so it stays cheap while a request
// path holds the write lock.
func TestSandboxTokenLookup(t *testing.T) {
	dir := t.TempDir()
	log := logrus.New()
	log.SetLevel(logrus.ErrorLevel)
	cfg := Config{Dir: dir, MaxFileMB: 8, MaxTotalMB: 64, MaxAge: time.Hour,
		FsyncBatch: 8, FsyncInterval: 20 * time.Millisecond, Metrics: NopMetrics(), Log: log}

	r1, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := r1.RecordSync(ctx, Event{Type: TypeSandboxCreate, Sandbox: "sb-1", Token: "owner"}); err != nil {
		t.Fatal(err)
	}
	// A later event for the same sandbox must not change the owner.
	if err := r1.RecordSync(ctx, Event{Type: TypeExec, Sandbox: "sb-1", Token: "owner"}); err != nil {
		t.Fatal(err)
	}
	if err := r1.RecordSync(ctx, Event{Type: TypeSandboxCreate, Sandbox: "sb-2", Token: "other"}); err != nil {
		t.Fatal(err)
	}

	if tok, ok := r1.SandboxToken("sb-1"); !ok || tok != "owner" {
		t.Fatalf("after append: SandboxToken(sb-1) = %q,%v want owner,true", tok, ok)
	}
	if tok, ok := r1.SandboxToken("sb-2"); !ok || tok != "other" {
		t.Fatalf("after append: SandboxToken(sb-2) = %q,%v want other,true", tok, ok)
	}
	if _, ok := r1.SandboxToken("sb-none"); ok {
		t.Fatal("SandboxToken for an unknown sandbox must return false")
	}
	_ = r1.Close()

	// A fresh Recorder over the same dir rebuilds the map in recover().
	r2, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	if tok, ok := r2.SandboxToken("sb-1"); !ok || tok != "owner" {
		t.Fatalf("after recover: SandboxToken(sb-1) = %q,%v want owner,true", tok, ok)
	}
	if tok, ok := r2.SandboxToken("sb-2"); !ok || tok != "other" {
		t.Fatalf("after recover: SandboxToken(sb-2) = %q,%v want other,true", tok, ok)
	}
}

// TestQueryLimitSmallerThanMatches checks that Query returns exactly limit
// events and the right ones for both orders.
func TestQueryLimitSmallerThanMatches(t *testing.T) {
	dir := t.TempDir()
	writeSome(t, dir, 30) // seq 1..30, all match Sandbox filter below is mixed

	asc, err := Query(dir, Filter{Limit: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(asc) != 4 {
		t.Fatalf("asc limit=4 returned %d events", len(asc))
	}
	for i, e := range asc {
		if e.Seq != uint64(i+1) {
			t.Fatalf("asc event %d has seq %d, want %d", i, e.Seq, i+1)
		}
	}

	desc, err := Query(dir, Filter{Limit: 4, Desc: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(desc) != 4 {
		t.Fatalf("desc limit=4 returned %d events", len(desc))
	}
	want := []uint64{30, 29, 28, 27}
	for i, e := range desc {
		if e.Seq != want[i] {
			t.Fatalf("desc event %d has seq %d, want %d", i, e.Seq, want[i])
		}
	}
}
