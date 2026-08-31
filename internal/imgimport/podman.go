// Package imgimport turns an OCI container image into a Moat rootfs template.
// It pulls or loads the image with podman, records its digest, reads its
// config, exports the filesystem, adds the guest agent, and writes an ext4
// file. It needs no root. There is no loop mount.
package imgimport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// inspectImage is the subset of `podman inspect` output this package reads.
type inspectImage struct {
	ID           string   `json:"Id"`
	RepoDigests  []string `json:"RepoDigests"`
	Architecture string   `json:"Architecture"`
	Os           string   `json:"Os"`
	Config       struct {
		Env        []string `json:"Env"`
		Cmd        []string `json:"Cmd"`
		Entrypoint []string `json:"Entrypoint"`
		WorkingDir string   `json:"WorkingDir"`
	} `json:"Config"`
}

// checkPodman reports whether podman is on PATH.
func checkPodman() error {
	if _, err := exec.LookPath("podman"); err != nil {
		return fmt.Errorf("podman is required for template import")
	}
	return nil
}

// podmanPull fetches the linux/amd64 image for ref into local storage.
func podmanPull(ctx context.Context, ref string) error {
	out, err := exec.CommandContext(ctx, "podman", "pull", "--arch", "amd64", "--os", "linux", ref).CombinedOutput()
	if err != nil {
		return fmt.Errorf("podman pull %s: %v: %s", ref, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// podmanLoad imports an image archive and returns the loaded reference.
func podmanLoad(ctx context.Context, archive string) (string, error) {
	out, err := exec.CommandContext(ctx, "podman", "load", "-i", archive).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("podman load %s: %v: %s", archive, err, strings.TrimSpace(string(out)))
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if r, ok := strings.CutPrefix(line, "Loaded image: "); ok {
			return strings.TrimSpace(r), nil
		}
		if r, ok := strings.CutPrefix(line, "Loaded image(s): "); ok {
			return strings.TrimSpace(r), nil
		}
	}
	return "", fmt.Errorf("podman load %s: no image reference in output: %s", archive, strings.TrimSpace(string(out)))
}

// podmanInspect reads the image metadata for ref.
func podmanInspect(ctx context.Context, ref string) (inspectImage, error) {
	out, err := exec.CommandContext(ctx, "podman", "inspect", "--type", "image", ref).Output()
	if err != nil {
		return inspectImage{}, fmt.Errorf("podman inspect %s: %v", ref, err)
	}
	var arr []inspectImage
	if err := json.Unmarshal(out, &arr); err != nil {
		return inspectImage{}, fmt.Errorf("parse podman inspect %s: %w", ref, err)
	}
	if len(arr) == 0 {
		return inspectImage{}, fmt.Errorf("podman inspect %s: no result", ref)
	}
	return arr[0], nil
}

// parseImageConfig turns an inspected image into template exec defaults.
func parseImageConfig(im inspectImage) (env map[string]string, dir string, defaultArgv []string, warnings []string) {
	env = map[string]string{}
	for _, kv := range im.Config.Env {
		k, v, ok := strings.Cut(kv, "=")
		if ok {
			env[k] = v
		}
	}
	dir = im.Config.WorkingDir
	argv := append(append([]string{}, im.Config.Entrypoint...), im.Config.Cmd...)
	if len(argv) > 0 {
		defaultArgv = argv
	} else {
		warnings = append(warnings, "the image has no ENTRYPOINT or CMD, so every exec must send argv")
	}
	return env, dir, defaultArgv, warnings
}

// podmanExportRootfs writes the image filesystem into destDir.
func podmanExportRootfs(ctx context.Context, ref, destDir string) error {
	cidOut, err := exec.CommandContext(ctx, "podman", "create", ref, "/bin/true").Output()
	if err != nil {
		return fmt.Errorf("podman create %s: %v", ref, err)
	}
	cid := strings.TrimSpace(string(cidOut))
	defer exec.Command("podman", "rm", "-f", cid).Run()

	export := exec.CommandContext(ctx, "podman", "export", cid)
	untar := exec.CommandContext(ctx, "tar", "-x", "-C", destDir, "-f", "-")
	pipe, err := export.StdoutPipe()
	if err != nil {
		return err
	}
	untar.Stdin = pipe
	var untarErr bytes.Buffer
	untar.Stderr = &untarErr
	if err := untar.Start(); err != nil {
		return err
	}
	if err := export.Run(); err != nil {
		return fmt.Errorf("podman export %s: %v", cid, err)
	}
	if err := untar.Wait(); err != nil {
		return fmt.Errorf("extract %s: %v: %s", cid, err, strings.TrimSpace(untarErr.String()))
	}
	return nil
}
