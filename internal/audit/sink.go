package audit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// ErrSinkQueueFull is returned by an async Sink when its buffer is full.
var ErrSinkQueueFull = errors.New("audit sink queue full")

// Sink receives a copy of each audit event after the recorder writes it to disk.
// Implementations must not block the caller.
type Sink interface {
	Emit(e Event) error
	Close() error
}

// WebhookConfig configures a WebhookSink.
type WebhookConfig struct {
	URL       string
	Timeout   time.Duration // per-request HTTP timeout; default 5s
	QueueSize int           // buffered events before drop; default 1024
	Client    *http.Client  // optional custom HTTP client
	Log       *logrus.Logger
}

// WebhookSink forwards audit events to an HTTP endpoint as JSON POST requests.
// Delivery runs asynchronously on a background worker with a bounded queue.
type WebhookSink struct {
	url     string
	timeout time.Duration
	client  *http.Client
	log     *logrus.Logger

	mu       sync.Mutex
	closed   bool
	ch       chan Event
	wg       sync.WaitGroup
	closeOne sync.Once
	cancel   context.CancelFunc
	ctx      context.Context
}

// NewWebhookSink starts a background worker that posts events to cfg.URL.
func NewWebhookSink(cfg WebhookConfig) (*WebhookSink, error) {
	if cfg.URL == "" {
		return nil, errors.New("webhook URL is required")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 1024
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: cfg.Timeout}
	}
	log := cfg.Log
	if log == nil {
		log = logrus.New()
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &WebhookSink{
		url:     cfg.URL,
		timeout: cfg.Timeout,
		client:  client,
		log:     log,
		ch:      make(chan Event, cfg.QueueSize),
		ctx:     ctx,
		cancel:  cancel,
	}
	w.wg.Add(1)
	go w.loop()
	return w, nil
}

// Emit enqueues e for delivery. It drops the event when the queue is full.
func (w *WebhookSink) Emit(e Event) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errors.New("webhook sink closed")
	}
	select {
	case w.ch <- e:
		return nil
	default:
		return ErrSinkQueueFull
	}
}

// Close stops accepting events and waits up to 2 seconds for queued deliveries.
func (w *WebhookSink) Close() error {
	w.closeOne.Do(func() {
		w.mu.Lock()
		w.closed = true
		close(w.ch)
		w.mu.Unlock()

		done := make(chan struct{})
		go func() {
			w.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			w.cancel()
			<-done
		}
		w.cancel()
	})
	return nil
}

func (w *WebhookSink) loop() {
	defer w.wg.Done()
	for e := range w.ch {
		if err := w.send(e); err != nil {
			w.log.WithError(err).WithField("seq", e.Seq).Warn("audit webhook delivery failed")
		}
	}
}

func (w *WebhookSink) send(e Event) error {
	body, err := marshalLine(e)
	if err != nil {
		return err
	}
	reqCtx, cancel := context.WithTimeout(w.ctx, w.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned status %d", resp.StatusCode)
	}
	return nil
}
