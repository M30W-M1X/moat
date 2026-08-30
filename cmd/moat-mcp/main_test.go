package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/M30W-M1X/moat/internal/moatapi"
)

// fakeMoatd is a stand-in for the daemon HTTP API.
func fakeMoatd(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]any{
			"id": "sb-test", "template": "default",
			"created_at": "2026-08-30T00:00:00Z", "expires_at": "2026-08-30T00:15:00Z",
			"networked": false,
		})
	})
	mux.HandleFunc("POST /v1/sandboxes/sb-test/exec", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Write([]byte(`{"stream":"stdout","data":"aGVsbG8K"}` + "\n")) // "hello\n"
		w.Write([]byte(`{"stream":"exit","code":0,"signal":0,"timed_out":false}` + "\n"))
	})
	mux.HandleFunc("PUT /v1/sandboxes/sb-test/files/{path...}", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.PathValue("path"), "secret") {
			w.WriteHeader(409)
			json.NewEncoder(w).Encode(map[string]any{
				"error": "secret detected in /work/secret — not sent",
				"hint":  "remove the credential",
				"findings": []map[string]any{
					{"rule": "aws-secret-key", "line": 1, "secret": "aZ****(40 chars)", "entropy": 5.1},
				},
			})
			return
		}
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{"bytes_written": 5, "sha256": "abc"})
	})
	mux.HandleFunc("DELETE /v1/sandboxes/sb-test", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(204)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func connectPair(t *testing.T, api *moatapi.Client) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	s := mcp.NewServer(&mcp.Implementation{Name: "moat", Version: "test"}, nil)
	registerTools(s, api, 4096)
	st, ct := mcp.NewInMemoryTransports()
	if _, err := s.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	c := mcp.NewClient(&mcp.Implementation{Name: "probe", Version: "test"}, nil)
	cs, err := c.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func firstText(res *mcp.CallToolResult) string {
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}

func TestMCPToolsListed(t *testing.T) {
	cs := connectPair(t, moatapi.New(fakeMoatd(t).URL, "t:x"))
	want := map[string]bool{
		"moat_create": false, "moat_list": false, "moat_destroy": false,
		"moat_exec": false, "moat_write_file": false, "moat_read_file": false,
	}
	for tool, err := range cs.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := want[tool.Name]; ok {
			want[tool.Name] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("tool %q not advertised", name)
		}
	}
}

func TestMCPCreateAndExec(t *testing.T) {
	cs := connectPair(t, moatapi.New(fakeMoatd(t).URL, "t:x"))
	ctx := context.Background()

	cr, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "moat_create", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if cr.IsError || !strings.Contains(firstText(cr), "sb-test") {
		t.Fatalf("create result: err=%v text=%q", cr.IsError, firstText(cr))
	}

	er, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "moat_exec", Arguments: map[string]any{
		"id": "sb-test", "argv": []string{"echo", "hello"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if er.IsError || !strings.Contains(firstText(er), "hello") || !strings.Contains(firstText(er), "exit 0") {
		t.Fatalf("exec result: %q", firstText(er))
	}
}

func TestMCPWriteFileSecretBlocked(t *testing.T) {
	cs := connectPair(t, moatapi.New(fakeMoatd(t).URL, "t:x"))
	ctx := context.Background()

	ok, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "moat_write_file", Arguments: map[string]any{
		"id": "sb-test", "path": "/work/ok.txt", "content": "hello",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if ok.IsError {
		t.Fatalf("clean write should succeed: %q", firstText(ok))
	}

	blocked, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "moat_write_file", Arguments: map[string]any{
		"id": "sb-test", "path": "/work/secret", "content": "AWS_SECRET_ACCESS_KEY=…",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !blocked.IsError {
		t.Fatal("secret write should be an error result")
	}
	txt := firstText(blocked)
	if !strings.Contains(txt, "aws-secret-key") || !strings.Contains(txt, "real credentials") {
		t.Fatalf("block message unhelpful: %q", txt)
	}
}
