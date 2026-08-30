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
// order when f.Desc is set.
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
	for _, file := range files {
		evs, _, err := scanFile(file)
		if err != nil {
			return nil, err
		}
		for _, e := range evs {
			if f.match(e) {
				out = append(out, e)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if f.Desc {
			return out[i].Seq > out[j].Seq
		}
		return out[i].Seq < out[j].Seq
	})
	if len(out) > limit {
		out = out[:limit]
	}
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
