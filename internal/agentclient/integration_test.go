//go:build integration

// Integration test: boots a real Firecracker microVM and drives the agent.
//
//	go test -tags integration ./internal/agentclient -run TestAgent -v
//
// It needs /dev/kvm, the firecracker binary on PATH, and the built images at
// build/artifacts/{vmlinux,rootfs.ext4} (run: make images).
package agentclient

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/M30W-M1X/microvm_controller/internal/config"
	"github.com/M30W-M1X/microvm_controller/internal/vm"
)

func bootForTest(t *testing.T) (*Client, func()) {
	t.Helper()
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("no /dev/kvm")
	}
	m, err := config.NewMachine("it-" + t.Name())
	if err != nil {
		t.Fatalf("NewMachine: %v", err)
	}
	if err := m.Validate(); err != nil {
		t.Skipf("images missing (run: make images): %v", err)
	}

	log := logrus.New()
	log.SetLevel(logrus.WarnLevel)

	ctx := context.Background()
	inst, err := vm.Boot(ctx, m, log)
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	cli, err := Dial(m.VsockUDSPath(), config.VsockPort)
	if err != nil {
		_ = inst.Stop(ctx)
		t.Fatalf("Dial: %v", err)
	}
	readyCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := cli.WaitReady(readyCtx); err != nil {
		cli.Close()
		_ = inst.Stop(ctx)
		t.Fatalf("WaitReady: %v", err)
	}
	return cli, func() {
		cli.Close()
		stopCtx, c := context.WithTimeout(context.Background(), 15*time.Second)
		defer c()
		_ = inst.Stop(stopCtx)
	}
}

func TestAgentExecAndFiles(t *testing.T) {
	cli, stop := bootForTest(t)
	defer stop()
	ctx := context.Background()

	t.Run("info", func(t *testing.T) {
		info, err := cli.Info(ctx)
		if err != nil {
			t.Fatalf("Info: %v", err)
		}
		if info.GetPid() != 1 {
			t.Errorf("guest pid = %d, want 1", info.GetPid())
		}
	})

	t.Run("exec_streams_and_exit_code", func(t *testing.T) {
		var out, errb bytes.Buffer
		res, err := cli.Exec(ctx, ExecOptions{
			Argv: []string{"sh", "-c", "echo to-stdout; echo to-stderr 1>&2; exit 3"},
		}, &out, &errb)
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if res.Code != 3 {
			t.Errorf("exit code = %d, want 3", res.Code)
		}
		if out.String() != "to-stdout\n" {
			t.Errorf("stdout = %q, want %q", out.String(), "to-stdout\n")
		}
		if errb.String() != "to-stderr\n" {
			t.Errorf("stderr = %q, want %q", errb.String(), "to-stderr\n")
		}
	})

	t.Run("exec_timeout", func(t *testing.T) {
		res, err := cli.Exec(ctx, ExecOptions{
			Argv:    []string{"sleep", "10"},
			Timeout: time.Second,
		}, nil, nil)
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if !res.TimedOut || res.Signal != 9 {
			t.Errorf("res = %+v, want TimedOut and signal 9", res)
		}
	})

	t.Run("file_round_trip", func(t *testing.T) {
		payload := make([]byte, 200000)
		_, _ = rand.Read(payload)
		want := sha256.Sum256(payload)

		w, err := cli.WriteFile(ctx, "/tmp/rt.bin", 0o644, bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if w.BytesWritten != uint64(len(payload)) || w.SHA256 != hex.EncodeToString(want[:]) {
			t.Fatalf("WriteResult = %+v, want %d bytes / %s", w, len(payload), hex.EncodeToString(want[:]))
		}

		var got bytes.Buffer
		r, err := cli.ReadFile(ctx, "/tmp/rt.bin", &got)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if r.SHA256 != w.SHA256 || !bytes.Equal(got.Bytes(), payload) {
			t.Fatalf("round trip mismatch: read %d bytes, sha %s", got.Len(), r.SHA256)
		}
	})

	t.Run("tty", func(t *testing.T) {
		var out bytes.Buffer
		_, err := cli.Exec(ctx, ExecOptions{
			Argv: []string{"sh", "-c", "test -t 1 && echo tty-ok"},
			TTY:  true,
		}, &out, nil)
		if err != nil {
			t.Fatalf("Exec tty: %v", err)
		}
		if !bytes.Contains(out.Bytes(), []byte("tty-ok")) {
			t.Errorf("tty output = %q, want it to contain tty-ok", out.String())
		}
	})
}
