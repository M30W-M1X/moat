package main

import (
	"bufio"
	"bytes"
	"strings"
)

// apkPackage is one row from /lib/apk/db/installed.
type apkPackage struct {
	Name    string
	Version string
	Arch    string
	License string
	Commit  string
}

// goModule is one dependency from `go version -m`.
type goModule struct {
	Path    string
	Version string
}

// parseAPKInstalled reads an apk "installed" database. Each record is a block of
// "K:value" lines. A blank line ends a record.
func parseAPKInstalled(raw []byte) []apkPackage {
	var out []apkPackage
	var cur apkPackage
	has := false
	flush := func() {
		if has && cur.Name != "" {
			out = append(out, cur)
		}
		cur, has = apkPackage{}, false
	}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			flush()
			continue
		}
		if len(line) < 2 || line[1] != ':' {
			continue
		}
		key, val := line[0], line[2:]
		has = true
		switch key {
		case 'P':
			cur.Name = val
		case 'V':
			cur.Version = val
		case 'A':
			cur.Arch = val
		case 'L':
			cur.License = val
		case 'c':
			cur.Commit = val
		}
	}
	flush()
	return out
}

// parseGoModules reads `go version -m <bin>` output and returns the "dep" rows.
// It skips the "mod" row (the main module) and the "build" rows.
func parseGoModules(raw []byte) []goModule {
	var out []goModule
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 3 && fields[0] == "dep" {
			out = append(out, goModule{Path: fields[1], Version: fields[2]})
		}
	}
	return out
}
