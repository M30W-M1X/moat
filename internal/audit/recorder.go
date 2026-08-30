package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// Metrics receives audit-writer counters. A server passes a Prometheus-backed
// implementation. Tests pass NopMetrics().
type Metrics interface {
	Recorded(typ, result string)
	Dropped()
	WriteObserved(path string, seconds float64)
	QueueDepth(n int)
}

type nopMetrics struct{}

func (nopMetrics) Recorded(string, string)       {}
func (nopMetrics) Dropped()                      {}
func (nopMetrics) WriteObserved(string, float64) {}
func (nopMetrics) QueueDepth(int)                {}

// NopMetrics returns a Metrics that discards everything.
func NopMetrics() Metrics { return nopMetrics{} }

// Config sets up a Recorder.
type Config struct {
	Dir           string // "" disables the recorder
	MaxFileMB     int
	MaxTotalMB    int
	MaxAge        time.Duration
	FsyncBatch    int
	FsyncInterval time.Duration
	Metrics       Metrics
	Log           *logrus.Logger
}

func (c *Config) setDefaults() {
	if c.MaxFileMB <= 0 {
		c.MaxFileMB = 128
	}
	if c.MaxTotalMB <= 0 {
		c.MaxTotalMB = 2048
	}
	if c.MaxAge <= 0 {
		c.MaxAge = 720 * time.Hour
	}
	if c.FsyncBatch <= 0 {
		c.FsyncBatch = 64
	}
	if c.FsyncInterval <= 0 {
		c.FsyncInterval = 200 * time.Millisecond
	}
	if c.Metrics == nil {
		c.Metrics = NopMetrics()
	}
	if c.Log == nil {
		c.Log = logrus.New()
	}
}

// Recorder appends events to the on-disk log. It is safe for concurrent use.
type Recorder struct {
	cfg     Config
	enabled bool

	mu      sync.Mutex // guards f, seq, tip, curSize, curName
	f       *os.File
	seq     uint64
	tip     string // hash of the last event written
	curName string
	curSize int64

	idx *index

	queue    chan Event
	writerWG sync.WaitGroup
	closed   chan struct{}
	closeOne sync.Once
}

// New opens the log in cfg.Dir. An empty Dir returns a disabled Recorder whose
// methods do nothing and always succeed.
func New(cfg Config) (*Recorder, error) {
	cfg.setDefaults()
	if cfg.Dir == "" {
		return &Recorder{cfg: cfg, enabled: false}, nil
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("audit dir: %w", err)
	}
	r := &Recorder{
		cfg:     cfg,
		enabled: true,
		tip:     zeroHash,
		idx:     newIndex(),
		queue:   make(chan Event, 8192),
		closed:  make(chan struct{}),
	}
	if err := r.recover(); err != nil {
		return nil, err
	}
	if err := r.openCurrent(); err != nil {
		return nil, err
	}
	r.writerWG.Add(1)
	go r.writeLoop()
	return r, nil
}

// Enabled reports whether the recorder writes anything.
func (r *Recorder) Enabled() bool { return r.enabled }

// RecordSync assigns the envelope fields, appends the event, and fsyncs before
// returning. A non-nil error means the event is NOT on disk.
func (r *Recorder) RecordSync(ctx context.Context, e Event) error {
	if !r.enabled {
		return nil
	}
	start := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	err := r.appendLocked(&e, true)
	r.cfg.Metrics.WriteObserved("sync", time.Since(start).Seconds())
	if err != nil {
		r.cfg.Metrics.Recorded(e.Type, "error")
		return err
	}
	r.cfg.Metrics.Recorded(e.Type, "ok")
	return nil
}

// appendLocked fills the chain fields, rotates if needed, writes the line, and
// optionally fsyncs. The caller holds r.mu.
func (r *Recorder) appendLocked(e *Event, sync bool) error {
	if err := r.rotateIfNeededLocked(); err != nil {
		return err
	}
	r.seq++
	e.Seq = r.seq
	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	}
	e.Prev = r.tip
	h, err := hashEvent(*e)
	if err != nil {
		r.seq--
		return err
	}
	e.Hash = h
	line, err := marshalLine(*e)
	if err != nil {
		r.seq--
		return err
	}
	n, err := r.f.Write(line)
	if err != nil {
		r.seq--
		return fmt.Errorf("append audit line: %w", err)
	}
	if sync {
		if err := r.f.Sync(); err != nil {
			// The line is on disk but not durable. Roll back the in-memory
			// state and remove the un-synced bytes so the chain does not fork.
			r.seq--
			_ = r.f.Truncate(r.curSize)
			_, _ = r.f.Seek(r.curSize, io.SeekStart)
			return fmt.Errorf("fsync audit log: %w", err)
		}
	}
	r.curSize += int64(n)
	r.tip = h
	r.idx.add(*e, r.curName, r.curSize-int64(n))
	return nil
}

// marshalLine renders e as its full JSON object plus a newline. The stored form
// is the canonical object with the hash field appended.
func marshalLine(e Event) ([]byte, error) {
	m := map[string]any{
		"seq": e.Seq, "ts": e.TS.UTC().Format(time.RFC3339Nano), "type": e.Type,
		"sandbox": e.Sandbox, "token": e.Token, "attrs": e.Attrs,
		"prev": e.Prev, "hash": e.Hash,
	}
	if e.Attrs == nil {
		m["attrs"] = map[string]any{}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Close drains the async buffer, fsyncs, and closes the file.
func (r *Recorder) Close() error {
	if !r.enabled {
		return nil
	}
	r.closeOne.Do(func() { close(r.closed) })
	r.writerWG.Wait()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	if err := r.f.Sync(); err != nil {
		return err
	}
	err := r.f.Close()
	r.f = nil
	return err
}

// Record enqueues e for the async writer. It never blocks. A full queue drops
// the event and increments the drop metric.
func (r *Recorder) Record(e Event) {
	if !r.enabled {
		return
	}
	select {
	case r.queue <- e:
		r.cfg.Metrics.QueueDepth(len(r.queue))
	default:
		r.cfg.Metrics.Dropped()
		r.cfg.Log.WithField("type", e.Type).Error("audit event dropped: async queue full")
	}
}

// writeLoop drains the async queue, batching fsyncs.
func (r *Recorder) writeLoop() {
	defer r.writerWG.Done()
	ticker := time.NewTicker(r.cfg.FsyncInterval)
	defer ticker.Stop()
	pending := 0
	flush := func() {
		if pending == 0 {
			return
		}
		r.mu.Lock()
		if r.f != nil {
			if err := r.f.Sync(); err != nil {
				r.cfg.Log.WithError(err).Error("audit batch fsync failed")
			}
		}
		r.mu.Unlock()
		pending = 0
	}
	drain := func() {
		for {
			select {
			case e := <-r.queue:
				r.writeOne(e)
				pending++
			default:
				return
			}
		}
	}
	for {
		select {
		case <-r.closed:
			drain()
			flush()
			return
		case e := <-r.queue:
			r.writeOne(e)
			pending++
			if pending >= r.cfg.FsyncBatch {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// writeOne appends one async event without a per-event fsync.
func (r *Recorder) writeOne(e Event) {
	start := time.Now()
	r.mu.Lock()
	err := r.appendLocked(&e, false)
	r.mu.Unlock()
	r.cfg.Metrics.WriteObserved("async", time.Since(start).Seconds())
	if err != nil {
		r.cfg.Metrics.Dropped()
		r.cfg.Metrics.Recorded(e.Type, "error")
		r.cfg.Log.WithError(err).WithField("type", e.Type).Error("audit async write failed")
		return
	}
	r.cfg.Metrics.Recorded(e.Type, "ok")
	r.cfg.Metrics.QueueDepth(len(r.queue))
}

func (r *Recorder) rotateIfNeededLocked() error {
	if r.f != nil && r.curSize < int64(r.cfg.MaxFileMB)<<20 &&
		r.curName == dailyName(time.Now(), 0) {
		return nil
	}
	return r.openCurrent()
}

func (r *Recorder) openCurrent() error {
	name := dailyName(time.Now(), 0)
	for n := 1; ; n++ {
		p := filepath.Join(r.cfg.Dir, name)
		info, err := os.Stat(p)
		if err != nil || info.Size() < int64(r.cfg.MaxFileMB)<<20 {
			f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				return fmt.Errorf("open audit file: %w", err)
			}
			if r.f != nil {
				_ = r.f.Close()
			}
			r.f, r.curName = f, name
			if info != nil {
				r.curSize = info.Size()
			} else {
				r.curSize = 0
			}
			return nil
		}
		name = dailyName(time.Now(), n)
	}
}

func dailyName(t time.Time, n int) string {
	if n == 0 {
		return fmt.Sprintf("audit-%s.jsonl", t.UTC().Format("2006-01-02"))
	}
	return fmt.Sprintf("audit-%s.%d.jsonl", t.UTC().Format("2006-01-02"), n)
}

// recover scans existing files for the highest seq and the chain tip, and
// rebuilds the index. Task 5/6 replace the body with the shared scanner.
func (r *Recorder) recover() error {
	files, err := listFiles(r.cfg.Dir)
	if err != nil {
		return err
	}
	for _, f := range files {
		evs, _, err := scanFile(f)
		if err != nil {
			return err
		}
		for _, e := range evs {
			if e.Seq > r.seq {
				r.seq = e.Seq
			}
			r.tip = e.Hash
			r.idx.add(e, filepath.Base(f), 0)
		}
	}
	return nil
}
