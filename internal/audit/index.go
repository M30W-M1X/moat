package audit

import (
	"sync"
	"time"
)

type locator struct {
	File   string
	Offset int64
}

// index keeps in-memory maps that the recorder fills on every append and on
// recover. The read path uses sandboxToken for an O(1) owner lookup. The
// locator maps bySandbox and byToken hold file offsets for a future seek index
// and no read path uses them yet.
type index struct {
	mu           sync.RWMutex
	bySandbox    map[string][]locator
	byToken      map[string][]locator
	sandboxToken map[string]string
	first        map[string]time.Time
	last         map[string]time.Time
}

func newIndex() *index {
	return &index{
		bySandbox:    map[string][]locator{},
		byToken:      map[string][]locator{},
		sandboxToken: map[string]string{},
		first:        map[string]time.Time{},
		last:         map[string]time.Time{},
	}
}

func (i *index) add(e Event, file string, off int64) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if e.Sandbox != "" {
		i.bySandbox[e.Sandbox] = append(i.bySandbox[e.Sandbox], locator{file, off})
		if t, ok := i.first[e.Sandbox]; !ok || e.TS.Before(t) {
			i.first[e.Sandbox] = e.TS
		}
		i.last[e.Sandbox] = e.TS
		if e.Token != "" {
			if _, ok := i.sandboxToken[e.Sandbox]; !ok {
				// The first event for a sandbox is sandbox.create. It carries
				// the owner token. Later events repeat the same token.
				i.sandboxToken[e.Sandbox] = e.Token
			}
		}
	}
	if e.Token != "" {
		i.byToken[e.Token] = append(i.byToken[e.Token], locator{file, off})
	}
}

// tokenFor returns the owner token id for a sandbox. The lookup is O(1) and
// takes no file scan.
func (i *index) tokenFor(sandbox string) (string, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	t, ok := i.sandboxToken[sandbox]
	return t, ok
}
