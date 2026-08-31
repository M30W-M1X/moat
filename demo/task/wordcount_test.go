package main

import (
	"strings"
	"testing"
)

func TestCountWords(t *testing.T) {
	got := countWords(strings.NewReader("The cat. The CAT sat on the mat!"))
	want := map[string]int{"the": 3, "cat": 2, "sat": 1, "on": 1, "mat": 1}

	if len(got) != len(want) {
		t.Fatalf("distinct words: got %d, want %d (%v)", len(got), len(want), got)
	}
	for w, n := range want {
		if got[w] != n {
			t.Errorf("word %q: got %d, want %d", w, got[w], n)
		}
	}
}

func TestCountWordsEmpty(t *testing.T) {
	if got := countWords(strings.NewReader("   \n\t  ")); len(got) != 0 {
		t.Fatalf("want no words, got %v", got)
	}
}
