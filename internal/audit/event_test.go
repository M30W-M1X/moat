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
		`"prev":"a91c4f","sandbox":"sb-7f3a19c4","seq":7,"token":"ci",` +
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
