package audit

import (
	"fmt"
	"time"
)

type ExecSummary struct {
	N          int   `json:"n"`
	Nonzero    int   `json:"nonzero"`
	Errors     int   `json:"errors"`
	TotalDurMS int64 `json:"total_dur_ms"`
}

type BytesSummary struct {
	In  int64 `json:"in"`
	Out int64 `json:"out"`
}

type FileRef struct {
	Dir    string `json:"dir"`
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
	Scan   string `json:"scan,omitempty"`
}

type SecretRef struct {
	Type   string   `json:"type"`
	Target string   `json:"target"`
	Rules  []string `json:"rules"`
}

type EgressSummary struct {
	Allow []string `json:"allow"`
	Deny  []string `json:"deny"`
}

type ChainRef struct {
	FirstSeq uint64 `json:"first_seq"`
	LastSeq  uint64 `json:"last_seq"`
	Verified bool   `json:"verified"`
}

// Report is the "what did this sandbox do" rollup.
type Report struct {
	Sandbox       string         `json:"sandbox"`
	Token         string         `json:"token"`
	Template      string         `json:"template"`
	CreatedAt     time.Time      `json:"created_at"`
	DestroyedAt   time.Time      `json:"destroyed_at"`
	DestroyReason string         `json:"destroy_reason"`
	LifetimeS     int64          `json:"lifetime_s"`
	Networked     bool           `json:"networked"`
	GuestIP       string         `json:"guest_ip"`
	Counts        map[string]int `json:"counts"`
	Exec          ExecSummary    `json:"exec"`
	Bytes         BytesSummary   `json:"bytes"`
	Files         []FileRef      `json:"files"`
	SecretEvents  []SecretRef    `json:"secret_events"`
	Egress        EgressSummary  `json:"egress"`
	Chain         ChainRef       `json:"chain"`
}

func attrStr(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func attrInt(m map[string]any, k string) int64 {
	switch v := m[k].(type) {
	case float64:
		return int64(v)
	case int:
		return int64(v)
	case int64:
		return v
	}
	return 0
}

func attrBool(m map[string]any, k string) bool {
	b, _ := m[k].(bool)
	return b
}

// BuildReport folds one sandbox's events, in seq order, into a Report.
func BuildReport(events []Event) Report {
	r := Report{Counts: map[string]int{}}
	for _, e := range events {
		r.Counts[e.Type]++
		if r.Sandbox == "" {
			r.Sandbox, r.Token = e.Sandbox, e.Token
		}
		switch e.Type {
		case TypeSandboxCreate:
			r.CreatedAt = e.TS
			r.Template = attrStr(e.Attrs, "template")
			r.Networked = attrBool(e.Attrs, "networked")
			r.GuestIP = attrStr(e.Attrs, "guest_ip")
		case TypeSandboxDestroy:
			r.DestroyedAt = e.TS
			r.DestroyReason = attrStr(e.Attrs, "reason")
			r.LifetimeS = attrInt(e.Attrs, "lifetime_s")
		case TypeExec:
			r.Exec.N++
			r.Exec.TotalDurMS += attrInt(e.Attrs, "dur_ms")
			if attrStr(e.Attrs, "error") != "" {
				r.Exec.Errors++
			} else if attrInt(e.Attrs, "exit") != 0 {
				r.Exec.Nonzero++
			}
		case TypeFilePut:
			r.Bytes.In += attrInt(e.Attrs, "bytes")
			r.Files = append(r.Files, FileRef{Dir: "in", Path: attrStr(e.Attrs, "path"),
				Bytes: attrInt(e.Attrs, "bytes"), SHA256: attrStr(e.Attrs, "sha256"),
				Scan: attrStr(e.Attrs, "scan")})
		case TypeFileGet:
			r.Bytes.Out += attrInt(e.Attrs, "bytes")
			r.Files = append(r.Files, FileRef{Dir: "out", Path: attrStr(e.Attrs, "path"),
				Bytes: attrInt(e.Attrs, "bytes"), SHA256: attrStr(e.Attrs, "sha256")})
		case TypeSecretBlock, TypeSecretAllow, TypeSecretRedact:
			r.SecretEvents = append(r.SecretEvents, SecretRef{
				Type: e.Type, Target: attrStr(e.Attrs, "target"), Rules: attrStrSlice(e.Attrs, "rules")})
		case TypeEgressAllow:
			r.Egress.Allow = append(r.Egress.Allow, hostPort(e.Attrs))
		case TypeEgressDeny:
			r.Egress.Deny = append(r.Egress.Deny, hostPort(e.Attrs))
		}
	}
	if len(events) > 0 {
		r.Chain.FirstSeq = events[0].Seq
		r.Chain.LastSeq = events[len(events)-1].Seq
	}
	return r
}

func hostPort(m map[string]any) string {
	return fmt.Sprintf("%s:%d", attrStr(m, "host"), attrInt(m, "port"))
}

func attrStrSlice(m map[string]any, k string) []string {
	raw, _ := m[k].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
