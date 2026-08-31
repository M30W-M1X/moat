package audit

import (
	"fmt"
	"sort"
	"time"
)

// Filter narrows a Query. A zero field does not filter.
type Filter struct {
	Sandbox string
	Token   string
	Types   []string
	Since   time.Time
	Until   time.Time
	Limit   int
	Desc    bool
}

func (f Filter) match(e Event) bool {
	if f.Sandbox != "" && e.Sandbox != f.Sandbox {
		return false
	}
	if f.Token != "" && e.Token != f.Token {
		return false
	}
	if len(f.Types) > 0 {
		hit := false
		for _, t := range f.Types {
			if e.Type == t {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	if !f.Since.IsZero() && e.TS.Before(f.Since) {
		return false
	}
	if !f.Until.IsZero() && e.TS.After(f.Until) {
		return false
	}
	return true
}

const (
	defaultLimit = 1000
	maxLimit     = 100000
)

// Query returns the events in dir that match f, in seq order. It reverses the
// order when f.Desc is set. Query holds at most limit events during the scan,
// so an unfiltered query over a full retention-cap log stays bounded.
func Query(dir string, f Filter) ([]Event, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	files, err := listFiles(dir)
	if err != nil {
		return nil, err
	}
	var out []Event

	if f.Desc {
		// Walk files newest name first and lines back to front. Stop once the
		// window holds limit events. Newer names sort after older names.
		for i := len(files) - 1; i >= 0 && len(out) < limit; i-- {
			evs, _, err := scanFile(files[i])
			if err != nil {
				return nil, err
			}
			for j := len(evs) - 1; j >= 0 && len(out) < limit; j-- {
				if f.match(evs[j]) {
					out = append(out, evs[j])
				}
			}
		}
		// A final stable sort guards against duplicate-seq corruption.
		sort.SliceStable(out, func(a, b int) bool { return out[a].Seq > out[b].Seq })
		return out, nil
	}

	// Ascending. Files and lines are already in seq order. Keep the first
	// limit matches, then stop.
	for _, file := range files {
		if len(out) >= limit {
			break
		}
		evs, _, err := scanFile(file)
		if err != nil {
			return nil, err
		}
		for _, e := range evs {
			if f.match(e) {
				out = append(out, e)
				if len(out) >= limit {
					break
				}
			}
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].Seq < out[b].Seq })
	return out, nil
}

// VerifyResult is the outcome of a chain check.
type VerifyResult struct {
	OK           bool
	Events       int
	FirstSeq     uint64
	LastSeq      uint64
	FirstTS      time.Time
	LastTS       time.Time
	PrunedPrefix bool
	Problems     []string
}

// Verify recomputes every hash and checks every prev link and seq step across
// all files in dir. A truncated tail line is a crash artefact. scanFile drops
// it. The intact prefix still verifies and OK stays true.
func Verify(dir string) (VerifyResult, error) {
	files, err := listFiles(dir)
	if err != nil {
		return VerifyResult{}, err
	}
	var all []Event
	for _, file := range files {
		evs, _, err := scanFile(file)
		if err != nil {
			return VerifyResult{}, err
		}
		all = append(all, evs...)
	}
	return VerifyEvents(all), nil
}

// VerifyEvents recomputes every hash and checks every prev link and seq step
// across evs in order. The caller gives the events in seq order. A pruned
// prefix keeps OK true. The first event with a non-zero Prev marks the prune.
func VerifyEvents(evs []Event) VerifyResult {
	var res VerifyResult
	var prevHash string
	var prevSeq uint64
	first := true
	for _, e := range evs {
		want, herr := hashEvent(e)
		if herr != nil {
			res.Problems = append(res.Problems, fmt.Sprintf("hash error at seq %d: %v", e.Seq, herr))
			continue
		}
		if e.Hash != want {
			res.Problems = append(res.Problems, fmt.Sprintf("hash mismatch at seq %d", e.Seq))
		}
		if first {
			res.FirstSeq, res.FirstTS = e.Seq, e.TS
			if e.Prev != zeroHash {
				res.PrunedPrefix = true
			}
			first = false
		} else {
			if e.Seq != prevSeq+1 {
				res.Problems = append(res.Problems, fmt.Sprintf("seq gap: %d then %d", prevSeq, e.Seq))
			}
			if e.Prev != prevHash {
				res.Problems = append(res.Problems, fmt.Sprintf("prev break at seq %d", e.Seq))
			}
		}
		prevHash, prevSeq = e.Hash, e.Seq
		res.LastSeq, res.LastTS = e.Seq, e.TS
		res.Events++
	}
	res.OK = len(res.Problems) == 0
	return res
}
