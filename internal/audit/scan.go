package audit

import (
	"bufio"
	"encoding/json"
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
// was truncated (a crash mid-write). Such a line is dropped.
func scanFile(path string) ([]Event, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return out, false, nil // truncated or corrupt tail
		}
		out = append(out, e)
	}
	return out, true, sc.Err()
}
