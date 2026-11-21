package brain

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/snapp-incubator/snappcloud-bot/internal/agent"
)

type pardisTestRunner struct{}

func (pardisTestRunner) Run(ctx context.Context, in agent.Input) (string, error) {
	if len(in.Clusters) != 1 {
		return "", fmt.Errorf("unexpected cluster count %d", len(in.Clusters))
	}
	m := in.Clusters[0].MCP
	tools, err := m.ListTools(ctx)
	if err != nil {
		return "", err
	}
	if len(tools) != 1 || tools[0].Name != "whoami" {
		return "", fmt.Errorf("unexpected tools %v", tools)
	}
	if _, err = m.CallTool(ctx, "write", nil); err == nil {
		return "", fmt.Errorf("write was reachable")
	}
	return m.CallTool(ctx, "whoami", map[string]any{})
}

func TestPardisUsesEachCallersGrant(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token != "alice" && token != "bob" {
			t.Error("invalid delegated token")
		}
		var rpc struct {
			Method string
			ID     int
		}
		if err := json.NewDecoder(r.Body).Decode(&rpc); err != nil {
			t.Error(err)
			return
		}
		var result any
		switch rpc.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "session-"+token)
		default:
			if r.Header.Get("Mcp-Session-Id") != "session-"+token {
				t.Error("session crossed accounts")
			}
			switch rpc.Method {
			case "notifications/initialized":
				w.WriteHeader(http.StatusAccepted)
				return
			case "tools/list":
				result = map[string]any{"tools": []map[string]string{{"name": "whoami"}, {"name": "write"}}}
			case "tools/call":
				result = map[string]any{"content": []map[string]string{{"type": "text", "text": fmt.Sprintf(`{"email":"%s@example.test"}`, token)}}}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": result})
	}))
	defer server.Close()
	ctx := context.Background()
	b := Brain{agent: pardisTestRunner{}}
	var callers sync.WaitGroup
	for _, user := range []string{"alice", "bob"} {
		callers.Go(func() {
			email := user + "@example.test"
			if err := VerifyPardis(ctx, server.URL, user, email); err != nil {
				t.Error(err)
			}
			if err := VerifyPardis(ctx, server.URL, user, "other@example.test"); err == nil {
				t.Error("identity mismatch accepted")
			}
			answer, err := b.AnswerPardis(ctx, server.URL, user, email, "identity?", "", "req")
			if err != nil || !strings.Contains(answer, email) {
				t.Errorf("wrong answer for %s: %s %v", user, answer, err)
			}
		})
	}
	callers.Wait()
}
