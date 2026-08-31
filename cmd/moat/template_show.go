package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/M30W-M1X/moat/internal/config"
)

// loadSidecars reads every <dataDir>/*.template.json entry.
func loadSidecars(dataDir string) []templateEntry {
	matches, _ := filepath.Glob(filepath.Join(dataDir, "*.template.json"))
	var out []templateEntry
	for _, m := range matches {
		b, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		var e templateEntry
		if err := json.Unmarshal(b, &e); err != nil || e.Name == "" {
			continue
		}
		out = append(out, e)
	}
	return out
}

// templateShow implements `moat template show <name>`.
func templateShow(args []string) int {
	fs := flag.NewFlagSet("template show", flag.ContinueOnError)
	dataDir := fs.String("data", config.ArtifactsDir(), "directory that holds <name>.template.json")
	cfgPath := fs.String("config", "", "also look in this moatd.json")
	asJSON := fs.Bool("json", false, "print JSON")
	positionals, ok := parseInterspersed(fs, args)
	if !ok || len(positionals) != 1 {
		fmt.Fprintln(os.Stderr, "usage: moat template show <name>")
		return 2
	}
	want := positionals[0]

	for _, e := range loadSidecars(*dataDir) {
		if e.Name == want {
			return printEntry(e, *asJSON)
		}
	}
	if *cfgPath != "" {
		if e, ok := entryFromConfig(*cfgPath, want); ok {
			return printEntry(e, *asJSON)
		}
	}
	if want == "default" {
		return printEntry(templateEntry{
			Name:   "default",
			RootFS: filepath.Join(config.ArtifactsDir(), "rootfs.ext4"),
		}, *asJSON)
	}
	fmt.Fprintf(os.Stderr, "no template %q under %s\n", want, *dataDir)
	return 1
}

func entryFromConfig(path, name string) (templateEntry, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return templateEntry{}, false
	}
	var doc struct {
		Templates []templateEntry `json:"templates"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return templateEntry{}, false
	}
	for _, e := range doc.Templates {
		if e.Name == name {
			return e, true
		}
	}
	return templateEntry{}, false
}

func printEntry(e templateEntry, asJSON bool) int {
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(e)
		return 0
	}
	fmt.Printf("name         %s\n", e.Name)
	fmt.Printf("rootfs       %s\n", e.RootFS)
	if e.Source != "" {
		fmt.Printf("source       %s\n", e.Source)
	}
	if e.Workdir != "" {
		fmt.Printf("workdir      %s\n", e.Workdir)
	}
	if len(e.DefaultArgv) > 0 {
		fmt.Printf("default_argv %s\n", strings.Join(e.DefaultArgv, " "))
	}
	if len(e.Env) > 0 {
		fmt.Printf("env\n")
		for k, v := range e.Env {
			fmt.Printf("  %s=%s\n", k, v)
		}
	}
	return 0
}
