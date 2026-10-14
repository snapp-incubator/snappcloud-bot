package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/snapp-incubator/snappcloud-bot/internal/authzclient"
	"github.com/snapp-incubator/snappcloud-bot/internal/mcp"
)

func okAuthn() *authnStub {
	return &authnStub{
		id:    authzclient.Identity{User: "u@x", Region: "okd4-teh-1"},
		scope: authzclient.Scope{"okd4-teh-1": {Namespaces: []string{"team-a"}}},
	}
}

func rpc(t *testing.T, srvURL, token, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srvURL+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func resultOf(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	r, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", out)
	}
	return r
}

func textOf(t *testing.T, r map[string]any) string {
	t.Helper()
	c := r["content"].([]any)
	return c[0].(map[string]any)["text"].(string)
}

func TestMCPHandshakeAndToolList(t *testing.T) {
	srv := newSrv(t, okAuthn(), &fakeBrain{}, nil)
	defer srv.Close()

	_, out := rpc(t, srv.URL, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	r := resultOf(t, out)
	if r["protocolVersion"] != "2025-06-18" {
		t.Errorf("did not agree on the client's supported version: %v", r["protocolVersion"])
	}
	if _, ok := r["capabilities"].(map[string]any)["tools"]; !ok {
		t.Error("tools capability not advertised")
	}

	code, _ := rpc(t, srv.URL, "", `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if code != http.StatusAccepted {
		t.Errorf("notification answered with %d, want 202", code)
	}

	_, out = rpc(t, srv.URL, "", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	var names []string
	for _, tl := range resultOf(t, out)["tools"].([]any) {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "ask,whoami" {
		t.Errorf("tools: %v", names)
	}
}

// ask runs the same enforced, cleaned path as REST, and honours review.
func TestMCPAskAnswersCleanedAndHonoursReview(t *testing.T) {
	b := &fakeBrain{answer: "<think>internal</think>\n## Answer\n\n3 pods failing"}
	srv := newSrv(t, okAuthn(), b, nil)
	defer srv.Close()

	_, out := rpc(t, srv.URL, "tok", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ask","arguments":{"question":"what fails in team-a on okd4-teh-1?","review":true}}}`)
	r := resultOf(t, out)
	if r["isError"] != false {
		t.Fatalf("isError: %v", r)
	}
	if txt := textOf(t, r); strings.Contains(txt, "think") || !strings.HasPrefix(txt, "## Answer") {
		t.Errorf("answer not cleaned: %q", txt)
	}
	if !b.review {
		t.Error("review was requested but not passed to the brain")
	}
	if r["_meta"].(map[string]any)["requestId"] == "" {
		t.Error("no requestId to correlate with logs")
	}
	if b.gotUser != "u@x" {
		t.Errorf("ran as %q, not the token's identity", b.gotUser)
	}
}

// A refused caller gets a readable tool error, not an HTTP 401 that some
// clients would treat as a prompt to start an OAuth flow.
func TestMCPAskWithoutTokenIsAToolError(t *testing.T) {
	b := &fakeBrain{answer: "secret"}
	srv := newSrv(t, okAuthn(), b, nil)
	defer srv.Close()

	code, out := rpc(t, srv.URL, "", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"ask","arguments":{"question":"hi"}}}`)
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	r := resultOf(t, out)
	if r["isError"] != true || !strings.Contains(textOf(t, r), "bearer token") {
		t.Fatalf("expected a token error: %v", r)
	}
	if b.calls != 0 {
		t.Fatal("agent ran without a token")
	}
}

func TestMCPWhoami(t *testing.T) {
	srv := newSrv(t, okAuthn(), &fakeBrain{}, nil)
	defer srv.Close()
	_, out := rpc(t, srv.URL, "tok", `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"whoami","arguments":{}}}`)
	var w whoamiResponse
	if err := json.Unmarshal([]byte(textOf(t, resultOf(t, out))), &w); err != nil {
		t.Fatal(err)
	}
	if w.User != "u@x" || len(w.Clusters["okd4-teh-1"]) != 1 {
		t.Fatalf("whoami: %+v", w)
	}
}

func TestMCPProtocolErrors(t *testing.T) {
	srv := newSrv(t, okAuthn(), &fakeBrain{}, nil)
	defer srv.Close()

	_, out := rpc(t, srv.URL, "", `{"jsonrpc":"2.0","id":6,"method":"resources/list"}`)
	if e, ok := out["error"].(map[string]any); !ok || e["code"].(float64) != -32601 {
		t.Errorf("unknown method: %v", out)
	}
	resp, err := http.Get(srv.URL + "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /mcp: %d, want 405", resp.StatusCode)
	}
}

// REST errors carry the requestId too.
func TestQueryErrorCarriesRequestID(t *testing.T) {
	srv := newSrv(t, okAuthn(), &fakeBrain{}, nil)
	defer srv.Close()
	code, out := post(t, srv, "/v1/query", "tok", `{"query":""}`)
	if code != http.StatusBadRequest || out["requestId"] == "" || out["requestId"] == nil {
		t.Fatalf("status %d, body %v", code, out)
	}
}

// A real MCP client — the bot's own — completes the handshake, lists and calls.
func TestMCPInteropWithAnMCPClient(t *testing.T) {
	srv := newSrv(t, okAuthn(), &fakeBrain{answer: "all healthy"}, nil)
	defer srv.Close()

	c := mcp.New(srv.URL+"/mcp", "Bearer tok", false, time.Minute)
	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("tools: %d", len(tools))
	}
	out, err := c.CallTool(context.Background(), "ask", map[string]any{"question": "is team-a healthy?"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if out != "all healthy" {
		t.Fatalf("answer: %q", out)
	}
}
