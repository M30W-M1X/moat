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
	case "-h", "--help", "help":
		fmt.Fprint(os.Stderr, `moat template - inspect the rootfs templates a sandbox can start from

usage:
  moat template list     show the bundled template and where to add more

A template is a kernel plus a rootfs image. The daemon reads extra templates
from its -config file (see docs/moatd.example.json). Downloadable per-language
templates (moat template pull python, ...) are planned.
`)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "moat template: unknown subcommand %q (try: list)\n", sub)
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
