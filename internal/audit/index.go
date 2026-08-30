package audit

import (
	"sync"
	"time"
)

type locator struct {
	File   string
	Offset int64
}

type index struct {
	mu        sync.RWMutex
	bySandbox map[string][]locator
	byToken   map[string][]locator
	first     map[string]time.Time
	last      map[string]time.Time
}

func newIndex() *index {
	return &index{
		bySandbox: map[string][]locator{},
		byToken:   map[string][]locator{},
		first:     map[string]time.Time{},
		last:      map[string]time.Time{},
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
	}
	if e.Token != "" {
		i.byToken[e.Token] = append(i.byToken[e.Token], locator{file, off})
	}
}
