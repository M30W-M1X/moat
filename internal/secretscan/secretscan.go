// Package secretscan finds credentials in bytes that cross into a sandbox.
//
// It evaluates the Gitleaks rule set. The rule file gitleaks.toml is vendored
// from github.com/gitleaks/gitleaks (MIT, see gitleaks-LICENSE) and embedded.
// This package reads the rules directly. It does not import the Gitleaks
// module, which pulls in more than 200 dependencies.
//
// Each rule has a regular expression, an optional keyword prefilter, an
// optional Shannon-entropy floor, and optional allowlists. A match becomes a
// Finding. Redact replaces the matched spans.
package secretscan

import (
	_ "embed"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

//go:embed gitleaks.toml
var defaultRules string

// MaxScanBytes caps how much of one input the scanner reads. Content past this
// point is not scanned.
const MaxScanBytes = 8 << 20

// Finding is one detected secret.
type Finding struct {
	Rule        string  `json:"rule"`
	Description string  `json:"description"`
	Line        int     `json:"line"`   // 1-based line of the secret
	Match       string  `json:"-"`      // the full regexp match, for context
	Secret      string  `json:"secret"` // the captured secret, already masked
	Entropy     float64 `json:"entropy"`

	start, end int // byte offsets of the secret span; Redact replaces this range
}

// Scanner holds the compiled rule set.
type Scanner struct {
	rules   []rule
	global  []allowlist
	skipped int
}

type rule struct {
	id, desc    string
	re          *regexp.Regexp
	keywords    []string
	entropy     float64
	secretGroup int
	allowlists  []allowlist
}

type allowlist struct {
	regexes []*regexp.Regexp
	paths   []*regexp.Regexp
	stops   []string
	target  string // "match", "line", or "secret" (default)
}

// --- TOML shapes ---

type tomlConfig struct {
	Rules      []tomlRule      `toml:"rules"`
	Allowlist  *tomlAllowlist  `toml:"allowlist"`
	Allowlists []tomlAllowlist `toml:"allowlists"`
}

type tomlRule struct {
	ID          string           `toml:"id"`
	Description string           `toml:"description"`
	Regex       string           `toml:"regex"`
	Keywords    []string         `toml:"keywords"`
	Entropy     float64          `toml:"entropy"`
	SecretGroup int              `toml:"secretGroup"`
	Allowlist   *tomlAllowlist   `toml:"allowlist"`
	Allowlists  []tomlAllowlist  `toml:"allowlists"`
	Required    []toml.Primitive `toml:"required"` // multi-part rule; not supported
}

type tomlAllowlist struct {
	Regexes     []string `toml:"regexes"`
	Paths       []string `toml:"paths"`
	Stopwords   []string `toml:"stopwords"`
	RegexTarget string   `toml:"regexTarget"`
}

// New builds a Scanner from the embedded Gitleaks rules.
func New() (*Scanner, error) { return NewFromTOML(defaultRules) }

// NewFromTOML builds a Scanner from a Gitleaks-format rule file. Rules whose
// regular expression does not compile, and multi-part rules, are dropped and
// counted in Skipped.
func NewFromTOML(src string) (*Scanner, error) {
	var cfg tomlConfig
	if _, err := toml.Decode(src, &cfg); err != nil {
		return nil, err
	}
	s := &Scanner{}
	if cfg.Allowlist != nil {
		s.global = append(s.global, compileAllowlist(*cfg.Allowlist))
	}
	for _, a := range cfg.Allowlists {
		s.global = append(s.global, compileAllowlist(a))
	}
	for _, tr := range cfg.Rules {
		if len(tr.Required) > 0 || tr.Regex == "" {
			s.skipped++
			continue
		}
		re, err := regexp.Compile(tr.Regex)
		if err != nil {
			s.skipped++
			continue
		}
		r := rule{
			id: tr.ID, desc: tr.Description, re: re,
			entropy: tr.Entropy, secretGroup: tr.SecretGroup,
		}
		for _, k := range tr.Keywords {
			r.keywords = append(r.keywords, strings.ToLower(k))
		}
		if tr.Allowlist != nil {
			r.allowlists = append(r.allowlists, compileAllowlist(*tr.Allowlist))
		}
		for _, a := range tr.Allowlists {
			r.allowlists = append(r.allowlists, compileAllowlist(a))
		}
		s.rules = append(s.rules, r)
	}
	return s, nil
}

func compileAllowlist(a tomlAllowlist) allowlist {
	out := allowlist{target: a.RegexTarget}
	for _, p := range a.Regexes {
		if re, err := regexp.Compile(p); err == nil {
			out.regexes = append(out.regexes, re)
		}
	}
	for _, p := range a.Paths {
		if re, err := regexp.Compile(p); err == nil {
			out.paths = append(out.paths, re)
		}
	}
	for _, w := range a.Stopwords {
		out.stops = append(out.stops, strings.ToLower(w))
	}
	return out
}

// RuleCount returns how many rules are active.
func (s *Scanner) RuleCount() int { return len(s.rules) }

// Skipped returns how many rules were dropped at build time.
func (s *Scanner) Skipped() int { return s.skipped }

// Scan returns every secret in content. name is the target path, used only for
// path allowlists.
func (s *Scanner) Scan(name string, content []byte) []Finding {
	if len(content) > MaxScanBytes {
		content = content[:MaxScanBytes]
	}
	lower := strings.ToLower(string(content))

	// A path on the global allowlist is not scanned at all.
	for _, a := range s.global {
		for _, p := range a.paths {
			if p.MatchString(name) {
				return nil
			}
		}
	}

	var out []Finding
	seen := map[[3]int]bool{} // rule index + span, to dedupe

	for ri, r := range s.rules {
		if len(r.keywords) > 0 && !containsAny(lower, r.keywords) {
			continue
		}
		for _, loc := range r.re.FindAllSubmatchIndex(content, -1) {
			mStart, mEnd := loc[0], loc[1]
			sStart, sEnd := secretSpan(loc, r.secretGroup)
			secret := string(content[sStart:sEnd])
			match := string(content[mStart:mEnd])

			if r.entropy > 0 && shannon(secret) < r.entropy {
				continue
			}
			if s.allowed(name, match, secret, lineOf(content, mStart), r.allowlists) ||
				s.allowed(name, match, secret, lineOf(content, mStart), s.global) {
				continue
			}
			key := [3]int{ri, sStart, sEnd}
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, Finding{
				Rule: r.id, Description: r.desc,
				Line:    1 + strings.Count(string(content[:sStart]), "\n"),
				Match:   match,
				Secret:  mask(secret),
				Entropy: round1(shannon(secret)),
				start:   sStart, end: sEnd,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].start < out[j].start })
	return out
}

// Redact replaces every finding's match span in content with a marker. It
// returns new bytes and leaves content unchanged.
func (s *Scanner) Redact(content []byte, findings []Finding) []byte {
	spans := append([]Finding(nil), findings...)
	sort.Slice(spans, func(i, j int) bool { return spans[i].start > spans[j].start })
	out := append([]byte(nil), content...)
	for _, f := range spans {
		if f.start < 0 || f.end > len(out) || f.start >= f.end {
			continue
		}
		repl := []byte("***REDACTED:" + f.Rule + "***")
		out = append(out[:f.start], append(repl, out[f.end:]...)...)
	}
	return out
}

func (s *Scanner) allowed(name, match, secret, line string, lists []allowlist) bool {
	for _, a := range lists {
		for _, p := range a.paths {
			if p.MatchString(name) {
				return true
			}
		}
		lc := strings.ToLower(secret)
		for _, w := range a.stops {
			if strings.Contains(lc, w) {
				return true
			}
		}
		target := secret
		switch a.target {
		case "match":
			target = match
		case "line":
			target = line
		}
		for _, re := range a.regexes {
			if re.MatchString(target) || re.MatchString(secret) {
				return true
			}
		}
	}
	return false
}

// secretSpan returns the byte range of the captured secret. It prefers the
// configured group, then capture group 1, then the whole match.
func secretSpan(loc []int, group int) (int, int) {
	if group > 0 && 2*group+1 < len(loc) && loc[2*group] >= 0 {
		return loc[2*group], loc[2*group+1]
	}
	if len(loc) >= 4 && loc[2] >= 0 {
		return loc[2], loc[3]
	}
	return loc[0], loc[1]
}

func containsAny(haystack string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

func lineOf(content []byte, off int) string {
	start := off
	for start > 0 && content[start-1] != '\n' {
		start--
	}
	end := off
	for end < len(content) && content[end] != '\n' {
		end++
	}
	return string(content[start:end])
}

// mask keeps the shape of a secret without printing it. It shows the first two
// characters and the length.
func mask(s string) string {
	if len(s) <= 6 {
		return strings.Repeat("*", len(s))
	}
	return s[:2] + strings.Repeat("*", 4) + "(" + itoa(len(s)) + " chars)"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// shannon returns the per-character Shannon entropy of s, in bits.
func shannon(s string) float64 {
	if s == "" {
		return 0
	}
	var freq [256]float64
	n := 0
	for i := 0; i < len(s); i++ {
		freq[s[i]]++
		n++
	}
	var e float64
	for _, c := range freq {
		if c == 0 {
			continue
		}
		p := c / float64(n)
		e -= p * math.Log2(p)
	}
	return e
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }
