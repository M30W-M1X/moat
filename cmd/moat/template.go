package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/M30W-M1X/moat/internal/config"
)

// runTemplate implements `moat template <subcommand>`.
func runTemplate(args []string) int {
	sub := "list"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "list":
		return templateList()
	case "import":
		return templateImport(args[1:])
	case "-h", "--help", "help":
		fmt.Fprint(os.Stderr, `moat template - rootfs templates a sandbox can start from

usage:
  moat template list              show the templates this host knows
  moat template import <ref>      import an OCI image as a template
  moat template import --from-archive FILE
  moat template show <name>       print one template's detail

A template is a kernel plus a rootfs. The daemon reads templates from its
-config file (see docs/moatd.example.json). "moat template import" builds a
rootfs from a container image and records the image ENV, WORKDIR, and
ENTRYPOINT as exec defaults.
`)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "moat template: unknown subcommand %q (try: list, import, show)\n", sub)
		return 2
	}
}

func templateList() int {
	art := config.ArtifactsDir()
	kernel := filepath.Join(art, "vmlinux")
	rootfs := filepath.Join(art, "rootfs.ext4")

	state := "not built"
	if fileExists(kernel) && fileExists(rootfs) {
		state = "ready"
	}
	fmt.Printf("%-10s %-8s kernel=%s rootfs=%s\n", "default", state, kernel, rootfs)
	fmt.Println()
	fmt.Println("Build the default images with:  make images")
	fmt.Println("Add more templates in the daemon -config file (docs/moatd.example.json).")
	return 0
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
