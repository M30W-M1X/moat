// Command moat-mcp is a Model Context Protocol server for Moat. It gives an AI
// agent tools to create a disposable microVM sandbox, run commands in it, move
// files, and destroy it. It talks to a running moatd over its HTTP API.
//
//	MOAT_ADDR=http://127.0.0.1:8080 MOAT_TOKEN=ci:s3cr3t moat-mcp
//
// Point an MCP client at this binary over stdio. See SKILL.md.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/M30W-M1X/moat/internal/moatapi"
)

var version = "dev"

func main() {
	addr := envOr("MOAT_ADDR", "http://127.0.0.1:8080")
	socket := os.Getenv("MOAT_SOCKET")
	token := os.Getenv("MOAT_TOKEN")
	maxOut := 256 << 10

	for i := 1; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "-addr", "--addr":
			if i+1 < len(os.Args) {
				i++
				addr = os.Args[i]
			}
		case "-socket", "--socket":
			if i+1 < len(os.Args) {
				i++
				socket = os.Args[i]
			}
		case "-token", "--token":
			if i+1 < len(os.Args) {
				i++
				token = os.Args[i]
			}
		case "-version", "--version":
			fmt.Println("moat-mcp", version)
			return
		case "-h", "-help", "--help":
			fmt.Fprintln(os.Stderr, "usage: moat-mcp [-addr URL | -socket PATH] [-token id:secret]  (or $MOAT_ADDR, $MOAT_SOCKET, $MOAT_TOKEN)")
			return
		}
	}
	if token == "" {
		fmt.Fprintln(os.Stderr, "moat-mcp: no token. Set $MOAT_TOKEN or pass -token id:secret.")
		os.Exit(1)
	}

	var api *moatapi.Client
	if socket != "" {
		api = moatapi.NewUnix(socket, token)
	} else {
		api = moatapi.New(addr, token)
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "moat", Version: version}, nil)
	registerTools(s, api, maxOut)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := s.Run(ctx, &mcp.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "moat-mcp:", err)
		os.Exit(1)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// --- tool inputs / outputs ---

type createIn struct {
	Template   string `json:"template,omitempty" jsonschema:"template name; omit for the default"`
	Network    string `json:"network,omitempty" jsonschema:"deny (default, no network) or egress (network with the domain-filter proxy)"`
	TTLSeconds int    `json:"ttl_seconds,omitempty" jsonschema:"sandbox lifetime in seconds; omit for the server default"`
}
type sandboxOut struct {
	ID        string `json:"id"`
	Template  string `json:"template"`
	ExpiresAt string `json:"expires_at"`
	Networked bool   `json:"networked"`
	GuestIP   string `json:"guest_ip,omitempty"`
}

type idIn struct {
	ID string `json:"id" jsonschema:"the sandbox id from moat_create"`
}

type execIn struct {
	ID           string            `json:"id" jsonschema:"the sandbox id"`
	Argv         []string          `json:"argv" jsonschema:"the command and its arguments, e.g. [\"python\",\"main.py\"]"`
	Env          map[string]string `json:"env,omitempty" jsonschema:"extra environment variables"`
	Dir          string            `json:"dir,omitempty" jsonschema:"working directory in the guest"`
	Stdin        string            `json:"stdin,omitempty" jsonschema:"text piped to the command's stdin"`
	TimeoutSec   int               `json:"timeout_sec,omitempty" jsonschema:"kill the command after this many seconds; 0 means no limit"`
	AllowSecrets bool              `json:"allow_secrets,omitempty" jsonschema:"only if the user explicitly approved sending flagged content; never set this with the user's real credentials"`
}
type execOut struct {
	Exit      int    `json:"exit"`
	Signal    int    `json:"signal,omitempty"`
	TimedOut  bool   `json:"timed_out"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	Truncated bool   `json:"truncated,omitempty"`
}

type writeIn struct {
	ID           string `json:"id" jsonschema:"the sandbox id"`
	Path         string `json:"path" jsonschema:"absolute path in the guest, e.g. /work/main.py"`
	Content      string `json:"content,omitempty" jsonschema:"file contents as text"`
	ContentB64   string `json:"content_base64,omitempty" jsonschema:"file contents as base64, for binary data"`
	AllowSecrets bool   `json:"allow_secrets,omitempty" jsonschema:"only if the user explicitly approved; never set this for the user's real credentials"`
}
type writeOut struct {
	BytesWritten int64  `json:"bytes_written"`
	SHA256       string `json:"sha256"`
}

type readIn struct {
	ID   string `json:"id" jsonschema:"the sandbox id"`
	Path string `json:"path" jsonschema:"absolute path in the guest"`
}
type readOut struct {
	Path       string `json:"path"`
	SizeBytes  int    `json:"size_bytes"`
	Content    string `json:"content,omitempty"`
	ContentB64 string `json:"content_base64,omitempty"`
	IsBinary   bool   `json:"is_binary"`
}

type listOut struct {
	Sandboxes []sandboxOut `json:"sandboxes"`
}

// --- registration ---

func registerTools(s *mcp.Server, api *moatapi.Client, maxOut int) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "moat_create",
		Description: "Create a fresh, disposable, hardware-isolated microVM sandbox. Use one per task. Destroy it with moat_destroy when done.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in createIn) (*mcp.CallToolResult, sandboxOut, error) {
		sb, err := api.Create(ctx, moatapi.CreateOptions{
			Template:   in.Template,
			Networked:  strings.EqualFold(in.Network, "egress"),
			TTLSeconds: in.TTLSeconds,
		})
		if err != nil {
			return errResult(err), sandboxOut{}, nil
		}
		out := sandboxOut{sb.ID, sb.Template, sb.ExpiresAt.Format(time.RFC3339), sb.Networked, sb.GuestIP}
		return textResult(fmt.Sprintf("sandbox %s ready (template %s, expires %s)", sb.ID, sb.Template, out.ExpiresAt), out), out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "moat_list",
		Description: "List the sandboxes this token owns.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, listOut, error) {
		sbs, err := api.List(ctx)
		if err != nil {
			return errResult(err), listOut{}, nil
		}
		out := listOut{}
		for _, sb := range sbs {
			out.Sandboxes = append(out.Sandboxes, sandboxOut{sb.ID, sb.Template, sb.ExpiresAt.Format(time.RFC3339), sb.Networked, sb.GuestIP})
		}
		return textResult(fmt.Sprintf("%d sandbox(es)", len(out.Sandboxes)), out), out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "moat_destroy",
		Description: "Stop and discard a sandbox. Its disk is thrown away.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in idIn) (*mcp.CallToolResult, struct{}, error) {
		if err := api.Destroy(ctx, in.ID); err != nil {
			return errResult(err), struct{}{}, nil
		}
		return textResult("sandbox "+in.ID+" destroyed", nil), struct{}{}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "moat_exec",
		Description: "Run one command in a sandbox and wait for it to finish. Output is captured, not streamed.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in execIn) (*mcp.CallToolResult, execOut, error) {
		if len(in.Argv) == 0 {
			return errResult(errors.New("argv is required")), execOut{}, nil
		}
		res, err := api.Exec(ctx, in.ID, moatapi.ExecOptions{
			Argv:         in.Argv,
			Env:          in.Env,
			Dir:          in.Dir,
			Stdin:        []byte(in.Stdin),
			TimeoutMS:    in.TimeoutSec * 1000,
			AllowSecrets: in.AllowSecrets,
		}, maxOut)
		if err != nil {
			return errResult(err), execOut{}, nil
		}
		out := execOut{
			Exit: res.Exit, Signal: res.Signal, TimedOut: res.TimedOut,
			Stdout: string(res.Stdout), Stderr: string(res.Stderr), Truncated: res.Truncated,
		}
		summary := fmt.Sprintf("exit %d", res.Exit)
		if res.TimedOut {
			summary = "timed out"
		}
		text := summary + "\n--- stdout ---\n" + out.Stdout + "\n--- stderr ---\n" + out.Stderr
		return textResult(text, out), out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "moat_write_file",
		Description: "Write a file into a sandbox. The content is screened for credentials; a blocked write returns an error listing what was found.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in writeIn) (*mcp.CallToolResult, writeOut, error) {
		var data []byte
		if in.ContentB64 != "" {
			b, derr := base64.StdEncoding.DecodeString(in.ContentB64)
			if derr != nil {
				return errResult(fmt.Errorf("content_base64: %w", derr)), writeOut{}, nil
			}
			data = b
		} else {
			data = []byte(in.Content)
		}
		pr, err := api.PutFile(ctx, in.ID, in.Path, data, in.AllowSecrets)
		if err != nil {
			return errResult(err), writeOut{}, nil
		}
		out := writeOut{pr.BytesWritten, pr.SHA256}
		return textResult(fmt.Sprintf("wrote %d bytes to %s", pr.BytesWritten, in.Path), out), out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "moat_read_file",
		Description: "Read a file out of a sandbox. Text comes back as content; binary comes back base64.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in readIn) (*mcp.CallToolResult, readOut, error) {
		data, err := api.GetFile(ctx, in.ID, in.Path)
		if err != nil {
			return errResult(err), readOut{}, nil
		}
		out := readOut{Path: in.Path, SizeBytes: len(data)}
		if utf8.Valid(data) {
			out.Content = string(data)
		} else {
			out.IsBinary = true
			out.ContentB64 = base64.StdEncoding.EncodeToString(data)
		}
		return textResult(fmt.Sprintf("read %d bytes from %s", len(data), in.Path), out), out, nil
	})
}

// textResult builds a successful result with a human-readable text block and an
// optional JSON block.
func textResult(text string, structured any) *mcp.CallToolResult {
	content := []mcp.Content{&mcp.TextContent{Text: text}}
	if structured != nil {
		if b, err := json.Marshal(structured); err == nil {
			content = append(content, &mcp.TextContent{Text: string(b)})
		}
	}
	return &mcp.CallToolResult{Content: content}
}

// errResult turns an API error into an error result. A secret block gets a
// message that tells the agent to ask the user, not to override.
func errResult(err error) *mcp.CallToolResult {
	var sb *moatapi.SecretBlockedError
	if errors.As(err, &sb) {
		var lines []string
		for _, f := range sb.Findings {
			lines = append(lines, fmt.Sprintf("  %s at line %d (%s)", f.Rule, f.Line, f.Secret))
		}
		msg := "BLOCKED by the secret firewall: " + sb.Where + "\n" +
			strings.Join(lines, "\n") + "\n" +
			"Do not retry with allow_secrets unless the user has explicitly told you to send this. " +
			"Never put the user's real credentials into a sandbox."
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
	}
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}
}
