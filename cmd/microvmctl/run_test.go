package main

import "testing"

func TestSplitPair(t *testing.T) {
	cases := []struct {
		in   string
		a, b string
		ok   bool
	}{
		{"local:remote", "local", "remote", true},
		{"/a/b:/c/d", "/a/b", "/c/d", true},
		{"c:/win:/x", "c", "/win:/x", true}, // split at the first colon only
		{"noseparator", "", "", false},
	}
	for _, c := range cases {
		a, b, ok := splitPair(c.in)
		if a != c.a || b != c.b || ok != c.ok {
			t.Errorf("splitPair(%q) = (%q, %q, %v), want (%q, %q, %v)", c.in, a, b, ok, c.a, c.b, c.ok)
		}
	}
}

func TestParseEnv(t *testing.T) {
	if got := parseEnv(nil); got != nil {
		t.Errorf("parseEnv(nil) = %v, want nil", got)
	}
	got := parseEnv(repeatedFlag{"A=1", "B=two=parts", "C="})
	want := map[string]string{"A": "1", "B": "two=parts", "C": ""}
	if len(got) != len(want) {
		t.Fatalf("parseEnv len = %d, want %d (%v)", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("parseEnv[%q] = %q, want %q", k, got[k], v)
		}
	}
}
