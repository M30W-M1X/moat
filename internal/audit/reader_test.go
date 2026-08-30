package audit

import (
	"context"
	"os"
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
	log := logrus.New()
	log.SetLevel(logrus.ErrorLevel)
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
		t.Fatalf("need >= 2 files to test a pruned prefix, got %d", len(files))
	}
	// Retention removed the chain head. The remaining files still verify, but
	// the first surviving event's prev points at a hash that is gone.
	if err := os.Remove(files[0]); err != nil {
		t.Fatal(err)
	}
	res, err := Verify(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("verify should stay OK over a pruned prefix: %+v", res)
	}
	if !res.PrunedPrefix {
		t.Fatal("expected PrunedPrefix = true after removing the chain head file")
	}
}
