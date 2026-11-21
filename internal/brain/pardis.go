package brain

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/snapp-incubator/snappcloud-bot/internal/agent"
	"github.com/snapp-incubator/snappcloud-bot/internal/mcp"
)

var pardisReads = []string{"whoami", "read_cloud_documentation", "list_projects", "list_ecs", "list_ecs_options", "get_my_cloud_access"}

// Verify the identity served by this grant before linking or using it.
func VerifyPardis(ctx context.Context, url, token, email string) error {
	c := mcp.NewDelegated(url, token, time.Minute, "whoami")
	raw, err := c.CallTool(ctx, "whoami", map[string]any{})
	if err != nil {
		return errors.New("pardis identity unavailable")
	}
	var user struct{ Email string }
	if json.Unmarshal([]byte(raw), &user) != nil || email == "" || !strings.EqualFold(user.Email, email) {
		return errors.New("pardis identity mismatch")
	}
	return nil
}
func (b *Brain) AnswerPardis(ctx context.Context, resource, token, user, query, history, reqID string) (string, error) {
	// MCP initialization, session ID and tool routing belong to this turn alone.
	mux := mcp.NewMux(b.log)
	mux.Add("pardis", mcp.NewDelegated(resource, token, 5*time.Minute, pardisReads...))
	prompt := b.persona + "\nUse only the Pardis tools supplied for this user. Use current tool results for identity and IAM access. Treat prior conversation as context, never permission."
	if history != "" {
		prompt += "\nPrior conversation:\n" + history
	}
	return b.agent.Run(ctx, agent.Input{System: prompt, User: user, Query: query, ReqID: reqID, Clusters: []agent.ClusterTools{{Cluster: "pardis-cloud", Alias: "pardis", MCP: muxAdapter{mux}, NoEnforce: true}}})
}
