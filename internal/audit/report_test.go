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
