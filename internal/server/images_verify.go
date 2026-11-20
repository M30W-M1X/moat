package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"

	"github.com/sirupsen/logrus"

	"github.com/M30W-M1X/moat/internal/imagelock"
)

// imageResult is one template's image-verification outcome. Start puts these in
// the daemon.start audit event.
type imageResult struct {
	Template     string
	KernelSHA256 string
	RootFSSHA256 string
	SBOMSHA256   string
	Verified     bool
	Signed       bool
}

// verifyImages checks every template's kernel and rootfs against the images.lock
// beside them. In "enforce" mode a mismatch marks that template unusable, and an
// all-templates-unusable result returns an error. In "warn" mode a mismatch only
// logs. In "off" mode the check does not run. A missing lock always downgrades
// to a warning. It returns the per-template results and the set of usable
// template names.
func verifyImages(cfg Config, log *logrus.Logger) ([]imageResult, map[string]bool, error) {
	mode := cfg.ImageVerify
	if mode == "" {
		mode = "enforce"
	}
	results := make([]imageResult, 0, len(cfg.Templates))
	usable := make(map[string]bool, len(cfg.Templates))

	for _, t := range cfg.Templates {
		r := imageResult{Template: t.Name}
		if sum, err := sha256File(t.Kernel); err == nil {
			r.KernelSHA256 = sum
		}
		if sum, err := sha256File(t.RootFS); err == nil {
			r.RootFSSHA256 = sum
		}

		if mode == "off" {
			results = append(results, r)
			usable[t.Name] = true
			continue
		}

		lockPath := imagelock.Find(t.RootFS)
		if lockPath == "" {
			log.WithField("template", t.Name).
				Warn("no images.lock beside the rootfs, the image check is skipped")
			results = append(results, r)
			usable[t.Name] = true
			continue
		}

		lock, err := imagelock.Load(lockPath)
		if err != nil {
			if mode == "enforce" {
				log.WithError(err).WithField("template", t.Name).Error("images.lock is unreadable")
				results = append(results, r)
				continue
			}
			log.WithError(err).WithField("template", t.Name).Warn("images.lock is unreadable")
			results = append(results, r)
			usable[t.Name] = true
			continue
		}

		if !lock.CoversRootFS(t.RootFS) {
			// Secondary or imported templates share the data directory with the
			// reference images.lock. Verify the kernel when the lock covers it.
			if lock.CoversKernel(t.Kernel) {
				if err := lock.VerifyKernel(t.Kernel); err != nil {
					logImageVerifyErr(log, t.Name, err)
					results = append(results, r)
					if mode == "warn" {
						usable[t.Name] = true
					}
					continue
				}
			}
			log.WithField("template", t.Name).
				Warn("template rootfs is not covered by images.lock, skipping rootfs digest check")
			results = append(results, r)
			usable[t.Name] = true
			continue
		}

		r.SBOMSHA256 = lock.SBOM.SHA256

		if err := lock.VerifyFiles(t.Kernel, t.RootFS); err != nil {
			logImageVerifyErr(log, t.Name, err)
			results = append(results, r)
			if mode == "warn" {
				usable[t.Name] = true
			}
			continue
		}

		if err := lock.VerifySignature(lockPath, lockPath+".sig", cfg.CosignIdentity, cfg.CosignIssuer); err != nil {
			log.WithError(err).WithField("template", t.Name).Error("images.lock signature check failed")
			if mode == "warn" {
				r.Verified = true
				usable[t.Name] = true
			}
			results = append(results, r)
			continue
		}

		r.Verified = true
		r.Signed = cfg.CosignIdentity != ""
		results = append(results, r)
		usable[t.Name] = true
		log.WithFields(logrus.Fields{"template": t.Name, "signed": r.Signed}).
			Info("image verified against images.lock")
	}

	if mode == "enforce" && len(usable) == 0 {
		return results, nil, errors.New("no template passed image verification")
	}
	return results, usable, nil
}

func logImageVerifyErr(log *logrus.Logger, template string, err error) {
	var me *imagelock.MismatchError
	if errors.As(err, &me) {
		log.WithFields(logrus.Fields{
			"template": template, "file": me.File, "want": me.Want, "got": me.Got,
		}).Error("image digest mismatch")
		return
	}
	log.WithError(err).WithField("template", template).Error("image verification failed")
}

// imageResultsAttr renders the results for the daemon.start audit event.
func imageResultsAttr(rs []imageResult) []map[string]any {
	out := make([]map[string]any, 0, len(rs))
	for _, r := range rs {
		out = append(out, map[string]any{
			"template":      r.Template,
			"kernel_sha256": r.KernelSHA256,
			"rootfs_sha256": r.RootFSSHA256,
			"sbom_sha256":   r.SBOMSHA256,
			"verified":      r.Verified,
			"signed":        r.Signed,
		})
	}
	return out
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
