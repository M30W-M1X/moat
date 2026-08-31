package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestBuildBOMDeterministic(t *testing.T) {
	in := bomInput{
		SourceDateEpoch: 1788134400,
		RootfsSHA256:    "aaaa",
		BaseImage:       "docker.io/library/alpine@sha256:bbbb",
		KernelVersion:   "6.1.182",
		KernelSnapshot:  "20260826-abc-0",
		KernelSHA256:    "cccc",
		APK: []apkPackage{
			{Name: "musl", Version: "1.2.5-r0", Arch: "x86_64", License: "MIT"},
		},
		GoModules: []goModule{
			{Path: "google.golang.org/grpc", Version: "v1.71.0"},
		},
	}
	a, err := buildBOM(in)
	if err != nil {
		t.Fatal(err)
	}
	b, err := buildBOM(in)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("buildBOM is not deterministic for the same input")
	}

	var doc map[string]any
	if err := json.Unmarshal(a, &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if doc["bomFormat"] != "CycloneDX" || doc["specVersion"] != "1.5" {
		t.Fatalf("wrong header: %v %v", doc["bomFormat"], doc["specVersion"])
	}
	comps, _ := doc["components"].([]any)
	// alpine OS + 1 apk lib + guest-agent app + 1 go lib + kernel data = 5
	if len(comps) != 5 {
		t.Fatalf("got %d components, want 5", len(comps))
	}
	// The timestamp comes only from SourceDateEpoch.
	meta, _ := doc["metadata"].(map[string]any)
	if meta["timestamp"] != "2026-08-31T00:00:00Z" {
		t.Fatalf("timestamp = %v, want 2026-08-31T00:00:00Z", meta["timestamp"])
	}
}
