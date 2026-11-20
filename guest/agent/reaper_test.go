package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseProcStat(t *testing.T) {
	cases := []struct {
		name      string
		line      string
		wantState byte
		wantPPID  int
		wantOK    bool
	}{
		{
			name:      "zombie orphan",
			line:      "42 (sleep) Z 1 42 42 0 -1 4194304",
			wantState: 'Z',
			wantPPID:  1,
			wantOK:    true,
		},
		{
			name:      "comm with spaces and parens",
			line:      "99 (my (weird) proc) Z 1 99 99 0 -1",
			wantState: 'Z',
			wantPPID:  1,
			wantOK:    true,
		},
		{
			name:      "running child",
			line:      "100 (sh) S 55 100 100",
			wantState: 'S',
			wantPPID:  55,
			wantOK:    true,
		},
		{
			name:   "malformed",
			line:   "100 no-parens",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, ppid, ok := parseProcStat([]byte(tc.line))
			if ok != tc.wantOK || st != tc.wantState || ppid != tc.wantPPID {
				t.Fatalf("got (%q, %d, %v), want (%q, %d, %v)",
					st, ppid, ok, tc.wantState, tc.wantPPID, tc.wantOK)
			}
		})
	}
}

func TestReapOrphansSkipsTrackedChildren(t *testing.T) {
	proc := t.TempDir()
	mk := func(pid, stat string) {
		dir := filepath.Join(proc, pid)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("10", "10 (exec-child) Z 1 10 10")
	mk("20", "20 (orphan) Z 1 20 20")
	mk("30", "30 (running) S 1 30 30")
	mk("40", "40 (other-parent) Z 2 40 40")

	tr := &childTracker{active: map[int]struct{}{}}
	if _, err := tr.startAndTrack(func() (int, error) { return 10, nil }); err != nil {
		t.Fatal(err)
	}

	var reaped []int
	tr.reapOrphans(proc, func(pid int) { reaped = append(reaped, pid) })
	if len(reaped) != 1 || reaped[0] != 20 {
		t.Fatalf("reaped = %v, want [20]", reaped)
	}

	tr.untrack(10)
	reaped = nil
	tr.reapOrphans(proc, func(pid int) { reaped = append(reaped, pid) })
	if len(reaped) != 2 {
		t.Fatalf("after untrack reaped = %v, want [10 20]", reaped)
	}
}
