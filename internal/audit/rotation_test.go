package audit

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
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

func TestPruneBySizeKeepsNewestAndCurrent(t *testing.T) {
	dir := t.TempDir()
	// Three within-age files, ~600 KiB each, named oldest-first. mtime is
	// recent so only the size branch can evict them.
	line := func(seq int) []byte {
		pad := string(make([]byte, 600*1024))
		return []byte(`{"seq":` + strconv.Itoa(seq) + `,"ts":"2026-08-30T00:00:00Z","type":"exec","sandbox":"","token":"","attrs":{"pad":"` + pad + `"},"prev":"` + zeroHash + `","hash":"x"}` + "\n")
	}
	for i, name := range []string{"audit-2026-08-10.00000.jsonl", "audit-2026-08-11.00000.jsonl", "audit-2026-08-12.00000.jsonl"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, line(i+1), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	log := logrus.New()
	log.SetLevel(logrus.ErrorLevel)
	// budget = 1 MiB. The three old files total ~1.8 MiB, so prune must drop
	// the two oldest and keep the newest plus the current file.
	r, err := New(Config{Dir: dir, MaxFileMB: 1, MaxTotalMB: 1, MaxAge: 720 * time.Hour,
		Metrics: NopMetrics(), Log: log})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	cur := r.curName
	if err := r.prune(); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	files, _ := listFiles(dir)
	for _, f := range files {
		got[filepath.Base(f)] = true
	}
	if got["audit-2026-08-10.00000.jsonl"] || got["audit-2026-08-11.00000.jsonl"] {
		t.Fatalf("prune kept an evicted file: %v", files)
	}
	if !got["audit-2026-08-12.00000.jsonl"] {
		t.Fatal("prune evicted the newest non-current file")
	}
	if !got[cur] {
		t.Fatalf("prune removed the current file %q: %v", cur, files)
	}
}
