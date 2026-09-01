package imgimport

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Source names the image to import. Set exactly one field.
type Source struct {
	Ref     string // a registry reference, for example "python:3.12"
	Archive string // a path to a docker-save or OCI archive
}

// Options controls the import.
type Options struct {
	Name            string // template name and ext4 label
	SizeBytes       int64  // ext4 size; 0 means "size from the image"
	AgentPath       string // the guest-agent binary; required
	OutDir          string // the directory for <Name>-rootfs.ext4; required
	SourceDateEpoch int64  // 0 means the default
}

// Result reports what the import produced.
type Result struct {
	RootFSPath       string
	Source           string // "<repo>@sha256:..." or the image id
	Env              map[string]string
	Dir              string
	DefaultArgv      []string
	SizeBytes        int64
	Warnings         []string
	PackagesManifest []byte
}

// Import builds a Moat rootfs template from an OCI image.
func Import(ctx context.Context, src Source, opts Options) (*Result, error) {
	if err := checkPodman(); err != nil {
		return nil, err
	}
	if opts.Name == "" || opts.OutDir == "" || opts.AgentPath == "" {
		return nil, fmt.Errorf("Import needs Name, OutDir, and AgentPath")
	}

	ref := src.Ref
	usedLocal := false
	if src.Archive != "" {
		loaded, err := podmanLoad(ctx, src.Archive)
		if err != nil {
			return nil, err
		}
		ref = loaded
	} else {
		if err := podmanPull(ctx, ref); err != nil {
			// A pull can fail for a locally built image or an offline host.
			// Fall back to the image already in local storage.
			if !podmanImageExists(ctx, ref) {
				return nil, err
			}
			usedLocal = true
		}
	}

	im, err := podmanInspect(ctx, ref)
	if err != nil {
		return nil, err
	}
	if im.Architecture != "amd64" || (im.Os != "" && im.Os != "linux") {
		return nil, fmt.Errorf("image %s is %s/%s, need linux/amd64", ref, im.Os, im.Architecture)
	}

	res := &Result{}
	res.Env, res.Dir, res.DefaultArgv, res.Warnings = parseImageConfig(im)
	if usedLocal {
		res.Warnings = append(res.Warnings, "the pull failed, so the local image was used")
	}
	switch {
	case len(im.RepoDigests) > 0:
		res.Source = im.RepoDigests[0]
	default:
		res.Source = im.ID
		res.Warnings = append(res.Warnings, "the image has no registry digest, recording the local image id")
	}

	work, err := os.MkdirTemp("", "moat-imgimport-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	root := filepath.Join(work, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	if err := podmanExportRootfs(ctx, ref, root); err != nil {
		return nil, err
	}

	// A bare WORKDIR sets image metadata but does not always create the
	// directory on disk. moatd sets it as the exec working directory, so it
	// must exist. Join under root and clean, so ".." cannot escape.
	if res.Dir != "" {
		if err := os.MkdirAll(filepath.Join(root, filepath.Clean("/"+res.Dir)), 0o755); err != nil {
			return nil, fmt.Errorf("create workdir %s: %w", res.Dir, err)
		}
	}

	if b, err := os.ReadFile(filepath.Join(root, "lib", "apk", "db", "installed")); err == nil {
		res.PackagesManifest = b
	} else if b, err := os.ReadFile(filepath.Join(root, "var", "lib", "dpkg", "status")); err == nil {
		res.PackagesManifest = b
	}

	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return nil, err
	}
	out := filepath.Join(opts.OutDir, opts.Name+"-rootfs.ext4")
	size, warns, err := assembleRootfs(root, opts.AgentPath, out, opts.Name, opts.SourceDateEpoch, opts.SizeBytes)
	if err != nil {
		return nil, err
	}
	res.RootFSPath = out
	res.SizeBytes = size
	res.Warnings = append(res.Warnings, warns...)
	res.Warnings = dedupeStrings(res.Warnings)
	return res, nil
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// FormatWarnings joins warnings for a one-line log.
func FormatWarnings(w []string) string { return strings.Join(w, "; ") }
