package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/M30W-M1X/moat/internal/config"
	"github.com/M30W-M1X/moat/internal/imgimport"
)

// templateEntry is one moatd.json "templates" element that an import writes.
type templateEntry struct {
	Name        string            `json:"name"`
	RootFS      string            `json:"rootfs"`
	Env         map[string]string `json:"env,omitempty"`
	Workdir     string            `json:"workdir,omitempty"`
	DefaultArgv []string          `json:"default_argv,omitempty"`
	Source      string            `json:"source,omitempty"`
}

var nameRe = regexp.MustCompile(`^[a-z0-9-]+$`)

// deriveName picks a template name from an image reference.
func deriveName(ref string) (string, error) {
	s := ref
	if i := strings.Index(s, "@"); i >= 0 {
		s = s[:i]
	}
	slash := strings.LastIndex(s, "/")
	if c := strings.LastIndex(s, ":"); c > slash {
		s = s[:c]
	}
	if slash := strings.LastIndex(s, "/"); slash >= 0 {
		s = s[slash+1:]
	}
	s = strings.ToLower(s)
	if s == "" || len(s) > 16 || !nameRe.MatchString(s) {
		return "", fmt.Errorf("cannot derive a valid name from %q, pass --name (lower-case, [a-z0-9-], <=16)", ref)
	}
	return s, nil
}

// templateImport implements `moat template import`.
func templateImport(args []string) int {
	fs := flag.NewFlagSet("template import", flag.ContinueOnError)
	name := fs.String("name", "", "template name (default: derived from the image reference)")
	archive := fs.String("from-archive", "", "import this docker-save or OCI archive instead of a pull")
	sizeStr := fs.String("size", "", "ext4 size, for example 512M or 2G (default: from the image)")
	dataDir := fs.String("data", config.ArtifactsDir(), "directory for <name>-rootfs.ext4")
	agentPath := fs.String("agent", "", "guest-agent binary (default: auto-resolve)")
	cfgPath := fs.String("config", "", "append the template entry to this moatd.json")
	dryRun := fs.Bool("dry-run", false, "print the plan and do nothing")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	var src imgimport.Source
	switch {
	case *archive != "":
		src.Archive = *archive
	case fs.NArg() == 1:
		src.Ref = fs.Arg(0)
	default:
		fmt.Fprintln(os.Stderr, "usage: moat template import <image-ref> [flags]   (or --from-archive FILE)")
		return 2
	}

	tName := *name
	if tName == "" {
		derived, err := deriveName(firstNonEmpty(src.Ref, filepath.Base(src.Archive)))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		tName = derived
	} else if !nameRe.MatchString(tName) || len(tName) > 16 {
		fmt.Fprintf(os.Stderr, "invalid --name %q (lower-case, [a-z0-9-], <=16)\n", tName)
		return 1
	}

	var sizeBytes int64
	if *sizeStr != "" {
		n, err := parseSize(*sizeStr)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		sizeBytes = n
	}

	agent := *agentPath
	if agent == "" {
		agent = resolveAgent(*dataDir)
	}

	rootfsOut := filepath.Join(*dataDir, tName+"-rootfs.ext4")
	if *dryRun {
		fmt.Printf("would import %s\n", firstNonEmpty(src.Ref, src.Archive))
		fmt.Printf("  name    %s\n", tName)
		fmt.Printf("  rootfs  %s\n", rootfsOut)
		fmt.Printf("  agent   %s\n", agent)
		if *cfgPath != "" {
			fmt.Printf("  config  append to %s\n", *cfgPath)
		} else {
			fmt.Printf("  config  write %s\n", filepath.Join(*dataDir, tName+".template.json"))
		}
		return 0
	}

	res, err := imgimport.Import(context.Background(), src, imgimport.Options{
		Name: tName, SizeBytes: sizeBytes, AgentPath: agent, OutDir: *dataDir,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "import:", err)
		return 1
	}

	entry := templateEntry{
		Name: tName, RootFS: res.RootFSPath,
		Env: res.Env, Workdir: res.Dir, DefaultArgv: res.DefaultArgv, Source: res.Source,
	}

	var regMsg string
	if *cfgPath != "" {
		if err := registerTemplate(*cfgPath, entry); err != nil {
			fmt.Fprintln(os.Stderr, "register:", err)
			return 1
		}
		regMsg = "appended to " + *cfgPath
	} else {
		p, err := writeSidecar(*dataDir, entry)
		if err != nil {
			fmt.Fprintln(os.Stderr, "sidecar:", err)
			return 1
		}
		regMsg = "wrote " + p
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{
			"name": tName, "rootfs": res.RootFSPath, "source": res.Source,
			"size_bytes": res.SizeBytes, "env": res.Env, "workdir": res.Dir,
			"default_argv": res.DefaultArgv, "warnings": res.Warnings, "registered": regMsg,
		})
		return 0
	}

	fmt.Printf("imported %s\n", res.Source)
	fmt.Printf("  name         %s\n", tName)
	fmt.Printf("  rootfs       %s (%d MiB)\n", res.RootFSPath, res.SizeBytes>>20)
	if res.Dir != "" {
		fmt.Printf("  workdir      %s\n", res.Dir)
	}
	if len(res.DefaultArgv) > 0 {
		fmt.Printf("  default_argv %s\n", strings.Join(res.DefaultArgv, " "))
	}
	if len(res.Env) > 0 {
		fmt.Printf("  env          %d variables\n", len(res.Env))
	}
	for _, wmsg := range res.Warnings {
		fmt.Printf("  warning      %s\n", wmsg)
	}
	fmt.Printf("  %s\n", regMsg)
	if *cfgPath == "" {
		b, _ := json.MarshalIndent([]templateEntry{entry}, "  ", "  ")
		fmt.Printf("\nAdd this to the \"templates\" array of your moatd.json:\n  %s\n", b)
	}
	return 0
}

// registerTemplate appends e to the templates array of a moatd.json.
func registerTemplate(configPath string, e templateEntry) error {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", configPath, err)
	}
	var tmpls []json.RawMessage
	if r, ok := doc["templates"]; ok {
		if err := json.Unmarshal(r, &tmpls); err != nil {
			return fmt.Errorf("parse templates in %s: %w", configPath, err)
		}
	}
	for _, rt := range tmpls {
		var probe struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(rt, &probe)
		if probe.Name == e.Name {
			return fmt.Errorf("template %q already exists in %s (use --name)", e.Name, configPath)
		}
	}
	added, err := json.Marshal(e)
	if err != nil {
		return err
	}
	tmpls = append(tmpls, added)
	packed, err := json.Marshal(tmpls)
	if err != nil {
		return err
	}
	doc["templates"] = packed
	outBytes, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := configPath + ".tmp"
	if err := os.WriteFile(tmp, append(outBytes, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, configPath)
}

// writeSidecar writes a single template entry beside the rootfs.
func writeSidecar(dataDir string, e templateEntry) (string, error) {
	path := filepath.Join(dataDir, e.Name+".template.json")
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// resolveAgent finds the guest-agent binary for an import.
func resolveAgent(dataDir string) string {
	for _, p := range []string{
		filepath.Join("bin", "guest-agent"),
		filepath.Join(dataDir, "guest-agent"),
		filepath.Join(config.ArtifactsDir(), "guest-agent"),
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return filepath.Join(dataDir, "guest-agent") // let imgimport report the clear error
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// parseSize turns "512M" / "2G" / "1048576" into bytes.
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "G"), strings.HasSuffix(s, "g"):
		mult, s = 1<<30, s[:len(s)-1]
	case strings.HasSuffix(s, "M"), strings.HasSuffix(s, "m"):
		mult, s = 1<<20, s[:len(s)-1]
	case strings.HasSuffix(s, "K"), strings.HasSuffix(s, "k"):
		mult, s = 1<<10, s[:len(s)-1]
	}
	var n int64
	if _, err := fmt.Sscan(s, &n); err != nil || n <= 0 {
		return 0, fmt.Errorf("bad --size %q (use 512M, 2G, or a byte count)", s)
	}
	return n * mult, nil
}
