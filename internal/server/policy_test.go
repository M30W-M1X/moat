package server

import "testing"

func TestTokenPolicyTemplateAllowed(t *testing.T) {
	open := TokenPolicy{}
	if !open.templateAllowed("anything") {
		t.Error("empty Templates should allow any name")
	}
	p := TokenPolicy{Templates: []string{"a", "b"}}
	if !p.templateAllowed("a") || !p.templateAllowed("b") {
		t.Error("listed templates should be allowed")
	}
	if p.templateAllowed("c") {
		t.Error("unlisted template should be denied")
	}
}

func TestRegistryReservePerToken(t *testing.T) {
	r := newRegistry(0)

	// tokenMax 2 for "ci": third reserve fails.
	if err := r.reserve("ci", 2); err != nil {
		t.Fatalf("reserve 1: %v", err)
	}
	if err := r.reserve("ci", 2); err != nil {
		t.Fatalf("reserve 2: %v", err)
	}
	if err := r.reserve("ci", 2); err == nil {
		t.Fatal("reserve 3 should hit the per-token limit")
	}

	// A different token is independent.
	if err := r.reserve("dev", 2); err != nil {
		t.Fatalf("other token reserve: %v", err)
	}

	// Releasing frees a slot.
	r.release("ci")
	if err := r.reserve("ci", 2); err != nil {
		t.Fatalf("reserve after release: %v", err)
	}

	// tokenMax 0 means unlimited.
	for i := 0; i < 50; i++ {
		if err := r.reserve("bulk", 0); err != nil {
			t.Fatalf("unlimited reserve %d: %v", i, err)
		}
	}
}

func TestRegistryReserveGlobal(t *testing.T) {
	r := newRegistry(3)
	// The global check counts tracked sandboxes, so reserve then add.
	for i := 0; i < 3; i++ {
		if err := r.reserve("t", 0); err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
		r.add(&Sandbox{ID: string(rune('a' + i)), TokenID: "t"})
	}
	if err := r.reserve("t", 0); err == nil {
		t.Fatal("reserve past the global limit should fail")
	}
}
