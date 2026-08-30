package audit

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// rotateIfNeededLocked opens a fresh file when the current one is full or the
// UTC day changed. The caller holds r.mu. After a roll it prunes.
func (r *Recorder) rotateIfNeededLocked() error {
	today := dailyName(time.Now(), 0)
	if r.f != nil && r.curSize < int64(r.cfg.MaxFileMB)<<20 && sameDay(r.curName, today) {
		return nil
	}
	if err := r.openCurrent(); err != nil {
		return err
	}
	if err := r.prune(); err != nil {
		r.cfg.Log.WithError(err).Error("audit retention prune failed")
	}
	return nil
}

// sameDay reports whether name and today share the audit-YYYY-MM-DD prefix.
// The name is audit-YYYY-MM-DD.NNNNN.jsonl.
func sameDay(name, today string) bool {
	return len(name) >= 16 && len(today) >= 16 && name[:16] == today[:16]
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

// dailyName builds the file name for the given UTC day and roll counter. The
// counter is zero-padded so a plain name sort matches the write order. The
// first file of a day has counter 0.
func dailyName(t time.Time, n int) string {
	return fmt.Sprintf("audit-%s.%05d.jsonl", t.UTC().Format("2006-01-02"), n)
}

// prune deletes over-age files, then the oldest files while the total exceeds
// MaxTotalMB. It never deletes the current file.
func (r *Recorder) prune() error {
	files, err := listFiles(r.cfg.Dir)
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-r.cfg.MaxAge)
	var kept []string
	var total int64
	for _, f := range files {
		if filepath.Base(f) == r.curName {
			if fi, err := os.Stat(f); err == nil {
				total += fi.Size()
			}
			kept = append(kept, f)
			continue
		}
		fi, err := os.Stat(f)
		if err != nil {
			continue
		}
		if fi.ModTime().Before(cutoff) {
			_ = os.Remove(f)
			continue
		}
		total += fi.Size()
		kept = append(kept, f)
	}
	budget := int64(r.cfg.MaxTotalMB) << 20
	i := 0
	for total > budget && i < len(kept) {
		if filepath.Base(kept[i]) == r.curName {
			i++
			continue
		}
		if fi, err := os.Stat(kept[i]); err == nil {
			total -= fi.Size()
		}
		_ = os.Remove(kept[i])
		i++
	}
	return nil
}
