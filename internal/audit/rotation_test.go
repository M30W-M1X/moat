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
