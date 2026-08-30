// Package moatapi is a Go client for the moatd HTTP/JSON API. moat-mcp and, in
// future, the language SDKs use it.
package moatapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client talks to one moatd instance.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// New returns a Client. addr may be "host:port" or a full "http://host:port".
func New(addr, token string) *Client {
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	return &Client{
		base:  strings.TrimRight(addr, "/"),
		token: token,
		http:  &http.Client{Timeout: 0}, // per-call context sets the deadline
	}
}

// Sandbox mirrors the API's sandbox view.
type Sandbox struct {
	ID        string    `json:"id"`
	Template  string    `json:"template"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Networked bool      `json:"networked"`
	GuestIP   string    `json:"guest_ip"`
}

// CreateOptions are the fields of a create request.
type CreateOptions struct {
	Template   string
	Networked  bool
	TTLSeconds int
}

// ExecOptions are the fields of an exec request.
type ExecOptions struct {
	Argv         []string
	Env          map[string]string
	Dir          string
	Stdin        []byte
	TTY          bool
	TimeoutMS    int
	AllowSecrets bool
}

// ExecResult is the assembled outcome of a streamed exec.
type ExecResult struct {
	Stdout    []byte
	Stderr    []byte
	Exit      int
	Signal    int
	TimedOut  bool
	Truncated bool // output hit the cap
}

// PutResult is the outcome of a file write.
type PutResult struct {
	BytesWritten int64  `json:"bytes_written"`
	SHA256       string `json:"sha256"`
}

// Finding is one secret the firewall reported.
type Finding struct {
	Rule        string  `json:"rule"`
	Description string  `json:"description"`
	Line        int     `json:"line"`
	Secret      string  `json:"secret"`
	Entropy     float64 `json:"entropy"`
}

// APIError is a non-2xx response that is not a secret block.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("moatd %d: %s", e.Status, e.Message) }

// SecretBlockedError is a 409 from the ingress secret firewall.
type SecretBlockedError struct {
	Where    string    `json:"error"`
	Hint     string    `json:"hint"`
	Findings []Finding `json:"findings"`
}

func (e *SecretBlockedError) Error() string {
	rules := make([]string, 0, len(e.Findings))
	for _, f := range e.Findings {
		rules = append(rules, f.Rule)
	}
	return e.Where + " [" + strings.Join(rules, ", ") + "]"
}

// --- requests ---

func (c *Client) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.http.Do(req)
}

// decodeErr turns a non-2xx response into the right error type.
func decodeErr(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusConflict {
		var sb SecretBlockedError
		if json.Unmarshal(raw, &sb) == nil && len(sb.Findings) > 0 {
			return &sb
		}
	}
	var m struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &m)
	msg := m.Error
	if msg == "" {
		msg = strings.TrimSpace(string(raw))
	}
	return &APIError{Status: resp.StatusCode, Message: msg}
}

// Create starts a sandbox.
func (c *Client) Create(ctx context.Context, o CreateOptions) (Sandbox, error) {
	resp, err := c.do(ctx, http.MethodPost, "/v1/sandboxes", map[string]any{
		"template":    o.Template,
		"networked":   o.Networked,
		"ttl_seconds": o.TTLSeconds,
	})
	if err != nil {
		return Sandbox{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return Sandbox{}, decodeErr(resp)
	}
	var sb Sandbox
	return sb, json.NewDecoder(resp.Body).Decode(&sb)
}

// List returns the caller's sandboxes.
func (c *Client) List(ctx context.Context) ([]Sandbox, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/sandboxes", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, decodeErr(resp)
	}
	var out struct {
		Sandboxes []Sandbox `json:"sandboxes"`
	}
	return out.Sandboxes, json.NewDecoder(resp.Body).Decode(&out)
}

// Destroy stops a sandbox.
func (c *Client) Destroy(ctx context.Context, id string) error {
	resp, err := c.do(ctx, http.MethodDelete, "/v1/sandboxes/"+id, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return decodeErr(resp)
	}
	return nil
}

// Exec runs one command and assembles the NDJSON stream. maxOut caps each of
// stdout and stderr; 0 means 256 KiB.
func (c *Client) Exec(ctx context.Context, id string, o ExecOptions, maxOut int) (ExecResult, error) {
	if maxOut <= 0 {
		maxOut = 256 << 10
	}
	body := map[string]any{
		"argv":          o.Argv,
		"env":           o.Env,
		"dir":           o.Dir,
		"tty":           o.TTY,
		"timeout_ms":    o.TimeoutMS,
		"allow_secrets": o.AllowSecrets,
	}
	if len(o.Stdin) > 0 {
		body["stdin"] = base64.StdEncoding.EncodeToString(o.Stdin)
	}
	resp, err := c.do(ctx, http.MethodPost, "/v1/sandboxes/"+id+"/exec", body)
	if err != nil {
		return ExecResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ExecResult{}, decodeErr(resp)
	}

	var res ExecResult
	res.Exit = -1
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		var ev struct {
			Stream   string `json:"stream"`
			Data     string `json:"data"`
			Code     int    `json:"code"`
			Signal   int    `json:"signal"`
			TimedOut bool   `json:"timed_out"`
			Message  string `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Stream {
		case "stdout", "stderr":
			d, _ := base64.StdEncoding.DecodeString(ev.Data)
			dst := &res.Stdout
			if ev.Stream == "stderr" {
				dst = &res.Stderr
			}
			if len(*dst) < maxOut {
				room := maxOut - len(*dst)
				if len(d) > room {
					d = d[:room]
					res.Truncated = true
				}
				*dst = append(*dst, d...)
			} else {
				res.Truncated = true
			}
		case "exit":
			res.Exit, res.Signal, res.TimedOut = ev.Code, ev.Signal, ev.TimedOut
		case "error":
			return res, &APIError{Status: 0, Message: ev.Message}
		}
	}
	return res, sc.Err()
}

// PutFile writes content to path in the sandbox. It returns *SecretBlockedError
// when the firewall stops it.
func (c *Client) PutFile(ctx context.Context, id, path string, content []byte, allowSecrets bool) (PutResult, error) {
	p := "/v1/sandboxes/" + id + "/files/" + strings.TrimPrefix(path, "/")
	if allowSecrets {
		p += "?allow_secrets=true"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+p, bytes.NewReader(content))
	if err != nil {
		return PutResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return PutResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return PutResult{}, decodeErr(resp)
	}
	var pr PutResult
	return pr, json.NewDecoder(resp.Body).Decode(&pr)
}

// GetFile reads path from the sandbox.
func (c *Client) GetFile(ctx context.Context, id, path string) ([]byte, error) {
	p := "/v1/sandboxes/" + id + "/files/" + strings.TrimPrefix(path, "/")
	resp, err := c.do(ctx, http.MethodGet, p, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, decodeErr(resp)
	}
	return io.ReadAll(resp.Body)
}

// AuditEvent is one event returned by GET /v1/audit.
type AuditEvent struct {
	Seq     uint64         `json:"seq"`
	TS      time.Time      `json:"ts"`
	Type    string         `json:"type"`
	Sandbox string         `json:"sandbox"`
	Token   string         `json:"token"`
	Attrs   map[string]any `json:"attrs"`
	Prev    string         `json:"prev"`
	Hash    string         `json:"hash"`
}

// AuditFilter narrows an Audit call.
type AuditFilter struct {
	Sandbox string
	Token   string
	Types   []string
	Since   string
	Until   string
	Limit   int
	Desc    bool
}

// Report is the GET /v1/sandboxes/{id}/report body.
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
	Exec          struct {
		N          int   `json:"n"`
		Nonzero    int   `json:"nonzero"`
		Errors     int   `json:"errors"`
		TotalDurMS int64 `json:"total_dur_ms"`
	} `json:"exec"`
	Bytes struct {
		In  int64 `json:"in"`
		Out int64 `json:"out"`
	} `json:"bytes"`
	Files []struct {
		Dir    string `json:"dir"`
		Path   string `json:"path"`
		Bytes  int64  `json:"bytes"`
		SHA256 string `json:"sha256"`
		Scan   string `json:"scan"`
	} `json:"files"`
	SecretEvents []struct {
		Type   string   `json:"type"`
		Target string   `json:"target"`
		Rules  []string `json:"rules"`
	} `json:"secret_events"`
	Egress struct {
		Allow []string `json:"allow"`
		Deny  []string `json:"deny"`
	} `json:"egress"`
	Chain struct {
		FirstSeq uint64 `json:"first_seq"`
		LastSeq  uint64 `json:"last_seq"`
		Verified bool   `json:"verified"`
	} `json:"chain"`
}

// Audit reads events from GET /v1/audit.
func (c *Client) Audit(ctx context.Context, f AuditFilter) ([]AuditEvent, error) {
	q := url.Values{}
	if f.Sandbox != "" {
		q.Set("sandbox", f.Sandbox)
	}
	if f.Token != "" {
		q.Set("token", f.Token)
	}
	for _, t := range f.Types {
		q.Add("type", t)
	}
	if f.Since != "" {
		q.Set("since", f.Since)
	}
	if f.Until != "" {
		q.Set("until", f.Until)
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	if f.Desc {
		q.Set("order", "desc")
	}
	resp, err := c.do(ctx, http.MethodGet, "/v1/audit?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, decodeErr(resp)
	}
	var out []AuditEvent
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e AuditEvent
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// Report fetches the per-sandbox rollup.
func (c *Client) Report(ctx context.Context, id string) (Report, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/sandboxes/"+id+"/report", nil)
	if err != nil {
		return Report{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Report{}, decodeErr(resp)
	}
	var rep Report
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		return Report{}, err
	}
	return rep, nil
}
