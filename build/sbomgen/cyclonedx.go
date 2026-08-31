package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// bomInput carries everything buildBOM needs. The caller fills it from the
// rootfs, the agent binary, and the pinned kernel and base-image values.
type bomInput struct {
	SourceDateEpoch int64
	RootfsSHA256    string
	BaseImage       string // docker.io/library/alpine@sha256:...
	KernelVersion   string
	KernelSnapshot  string
	KernelSHA256    string
	APK             []apkPackage
	GoModules       []goModule
}

type cdxHash struct {
	Alg     string `json:"alg"`
	Content string `json:"content"`
}

type cdxLicense struct {
	License struct {
		ID string `json:"id"`
	} `json:"license"`
}

type cdxProp struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type cdxComponent struct {
	Type       string       `json:"type"`
	Name       string       `json:"name"`
	Version    string       `json:"version,omitempty"`
	PURL       string       `json:"purl,omitempty"`
	Licenses   []cdxLicense `json:"licenses,omitempty"`
	Hashes     []cdxHash    `json:"hashes,omitempty"`
	Properties []cdxProp    `json:"properties,omitempty"`
}

type cdxDoc struct {
	BOMFormat   string `json:"bomFormat"`
	SpecVersion string `json:"specVersion"`
	Version     int    `json:"version"`
	Metadata    struct {
		Timestamp string       `json:"timestamp"`
		Component cdxComponent `json:"component"`
	} `json:"metadata"`
	Components []cdxComponent `json:"components"`
}

func lic(id string) []cdxLicense {
	if id == "" {
		return nil
	}
	var l cdxLicense
	l.License.ID = id
	return []cdxLicense{l}
}

// buildBOM renders a CycloneDX 1.5 document. The output is byte-identical for
// the same input, so the build can hash it into images.lock.
func buildBOM(in bomInput) ([]byte, error) {
	ts := time.Unix(in.SourceDateEpoch, 0).UTC().Format(time.RFC3339)

	var comps []cdxComponent

	comps = append(comps, cdxComponent{
		Type: "operating-system",
		Name: "alpine",
		PURL: "pkg:oci/alpine@" + digestOf(in.BaseImage),
	})

	for _, p := range in.APK {
		comps = append(comps, cdxComponent{
			Type:     "library",
			Name:     p.Name,
			Version:  p.Version,
			PURL:     fmt.Sprintf("pkg:apk/alpine/%s@%s?arch=%s", p.Name, p.Version, p.Arch),
			Licenses: lic(p.License),
		})
	}

	comps = append(comps, cdxComponent{
		Type: "application",
		Name: "guest-agent",
		PURL: "pkg:golang/github.com/M30W-M1X/moat/guest/agent",
	})
	for _, m := range in.GoModules {
		comps = append(comps, cdxComponent{
			Type:    "library",
			Name:    m.Path,
			Version: m.Version,
			PURL:    fmt.Sprintf("pkg:golang/%s@%s", m.Path, m.Version),
		})
	}

	comps = append(comps, cdxComponent{
		Type:    "data",
		Name:    "vmlinux",
		Version: in.KernelVersion,
		Hashes:  []cdxHash{{Alg: "SHA-256", Content: in.KernelSHA256}},
		Properties: []cdxProp{
			{Name: "moat:ci-snapshot", Value: in.KernelSnapshot},
		},
	})

	sort.Slice(comps, func(i, j int) bool {
		if comps[i].PURL != comps[j].PURL {
			return comps[i].PURL < comps[j].PURL
		}
		return comps[i].Name < comps[j].Name
	})

	var doc cdxDoc
	doc.BOMFormat = "CycloneDX"
	doc.SpecVersion = "1.5"
	doc.Version = 1
	doc.Metadata.Timestamp = ts
	doc.Metadata.Component = cdxComponent{
		Type:   "container",
		Name:   "moat-rootfs",
		Hashes: []cdxHash{{Alg: "SHA-256", Content: in.RootfsSHA256}},
	}
	doc.Components = comps

	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// digestOf returns the "sha256:..." part of a "name@sha256:..." reference, or
// the whole string when there is no "@".
func digestOf(ref string) string {
	for i := len(ref) - 1; i >= 0; i-- {
		if ref[i] == '@' {
			return ref[i+1:]
		}
	}
	return ref
}
