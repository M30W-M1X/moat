package audit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// listFiles returns the audit files in dir, oldest name first.
func listFiles(dir string) ([]string, error) {
	m, err := filepath.Glob(filepath.Join(dir, "audit-*.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Strings(m)
	return m, nil
}

// scanFile reads every event in one file. The bool is false when the last line
// was truncated (a crash mid-write). Such a line is dropped. A bad line before
// the end means the file is corrupt and scanFile returns an error.
func scanFile(path string) ([]Event, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	badLine := -1
	var badErr error
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		if badLine >= 0 {
			// A readable line follows an unreadable one. The gap is not a
			// crash tail. The file is corrupt.
			return out, false, fmt.Errorf("corrupt audit line %d in %s: %w", badLine, path, badErr)
		}
		var e Event
		if uerr := json.Unmarshal(line, &e); uerr != nil {
			badLine, badErr = lineNo, uerr
			continue
		}
		out = append(out, e)
	}
	if serr := sc.Err(); serr != nil {
		return out, false, serr
	}
	if badLine >= 0 {
		// One trailing bad line is a crash artefact. Drop it.
		return out, false, nil
	}
	return out, true, nil
}
