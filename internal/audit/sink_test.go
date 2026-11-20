package audit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

func TestWebhookSinkReceivesSyncAndAsyncEvents(t *testing.T) {
	var mu sync.Mutex
	var got []Event

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		var e Event
		if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
			t.Errorf("decode webhook body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		got = append(got, e)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	log := logrus.New()
	log.SetLevel(logrus.ErrorLevel)

	sink, err := NewWebhookSink(WebhookConfig{URL: srv.URL, Log: log})
	if err != nil {
		t.Fatal(err)
	}

	rec, err := New(Config{
		Dir:           t.TempDir(),
		FsyncBatch:    1,
		FsyncInterval: 10 * time.Millisecond,
		Log:           log,
		Sinks:         []Sink{sink},
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := rec.RecordSync(context.Background(), Event{
		Type:    TypeSandboxCreate,
		Sandbox: "sb-1",
		Token:   "ci",
		Attrs:   map[string]any{"template": "default"},
	}); err != nil {
		t.Fatal(err)
	}
	rec.Record(Event{
		Type:    TypeExec,
		Sandbox: "sb-1",
		Token:   "ci",
		Attrs:   map[string]any{"exit": 0},
	})
	rec.Flush()
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("received %d webhook events, want 2: %+v", len(got), got)
	}
	if got[0].Type != TypeSandboxCreate || got[0].Hash == "" || got[0].Seq != 1 {
		t.Fatalf("first event wrong: %+v", got[0])
	}
	if got[1].Type != TypeExec || got[1].Prev != got[0].Hash || got[1].Seq != 2 {
		t.Fatalf("second event wrong: %+v", got[1])
	}
}

func TestWebhookSinkDropsWhenFull(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer func() {
		close(block)
		srv.Close()
	}()

	log := logrus.New()
	log.SetLevel(logrus.PanicLevel)
	sink, err := NewWebhookSink(WebhookConfig{
		URL:       srv.URL,
		QueueSize: 1,
		Timeout:   200 * time.Millisecond,
		Log:       log,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()

	// Fill worker + queue buffer and assert Emit never blocks and returns ErrSinkQueueFull.
	var sawFull bool
	for i := 0; i < 10; i++ {
		if err := sink.Emit(Event{Seq: uint64(i + 1), Type: TypeExec}); err == ErrSinkQueueFull {
			sawFull = true
			break
		}
	}
	if !sawFull {
		t.Fatal("expected ErrSinkQueueFull when webhook worker is blocked")
	}
}
