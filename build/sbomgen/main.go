// Command sbomgen writes a CycloneDX 1.5 SBOM for the built rootfs. Run it with
// `go run ./build/sbomgen`. It is never a shipped binary.
//
//	go run ./build/sbomgen \
//	  -rootfs build/artifacts/rootfs.ext4 -agent bin/guest-agent \
//	  -kernel-version 6.1.182 -kernel-snapshot 20260826-761f88fbb951-0 \
//	  -kernel-sha256 <hex> -base-image docker.io/library/alpine@sha256:<hex> \
//	  -source-date-epoch 1788134400 -out build/artifacts/sbom.cdx.json
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
)

func main() {
	rootfs := flag.String("rootfs", "build/artifacts/rootfs.ext4", "built rootfs image")
	agent := flag.String("agent", "bin/guest-agent", "guest-agent binary")
	kver := flag.String("kernel-version", "", "pinned kernel version, for example 6.1.182")
	ksnap := flag.String("kernel-snapshot", "", "pinned kernel CI snapshot")
	ksha := flag.String("kernel-sha256", "", "pinned kernel sha256")
	base := flag.String("base-image", "", "base image with digest")
	sde := flag.Int64("source-date-epoch", 0, "fixed build epoch")
	out := flag.String("out", "build/artifacts/sbom.cdx.json", "output path")
	flag.Parse()

	if *kver == "" || *ksnap == "" || *ksha == "" || *base == "" || *sde == 0 {
		fmt.Fprintln(os.Stderr, "sbomgen: -kernel-version, -kernel-snapshot, -kernel-sha256, -base-image, and -source-date-epoch are required")
		os.Exit(2)
	}

	rootfsSum, err := sha256File(*rootfs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sbomgen: hash rootfs:", err)
		os.Exit(1)
	}

	apkRaw, err := exec.Command("debugfs", "-R", "cat /lib/apk/db/installed", *rootfs).Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "sbomgen: read apk db with debugfs:", err)
		os.Exit(1)
	}
	modRaw, err := exec.Command("go", "version", "-m", *agent).Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "sbomgen: go version -m:", err)
		os.Exit(1)
	}

	apk := parseAPKInstalled(apkRaw)
	mods := parseGoModules(modRaw)

	doc, err := buildBOM(bomInput{
		SourceDateEpoch: *sde,
		RootfsSHA256:    rootfsSum,
		BaseImage:       *base,
		KernelVersion:   *kver,
		KernelSnapshot:  *ksnap,
		KernelSHA256:    *ksha,
		APK:             apk,
		GoModules:       mods,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "sbomgen: build:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, doc, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "sbomgen: write:", err)
		os.Exit(1)
	}
	// components = alpine OS + apk libs + guest-agent app + go libs + kernel.
	fmt.Printf("wrote %s (%d components)\n", *out, len(apk)+len(mods)+3)
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
