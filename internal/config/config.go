// Package config loads and validates the SnappCloud bot configuration.
//
// The bot connects to Mattermost (WebSocket), runs the in-process MCP agent, and
// authorizes each user's query by calling the per-region mcp-authz API. It holds
// no cluster credentials — authorization lives in mcp-authz, one per region.
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration document.
type Config struct {
	Mattermost Mattermost `yaml:"mattermost"`
	Memory     Memory     `yaml:"memory"`
	Authz      Authz      `yaml:"authz"`
	Agent      Agent      `yaml:"agent"`
	Limits     Limits     `yaml:"limits"`
	API        API        `yaml:"api"`
	Schedules  Schedules  `yaml:"schedules"`
}

// Schedules configures user-defined recurring queries. Each one costs an LLM
// run plus MCP calls every time it fires, so the limits are the important part.
type Schedules struct {
	// Enabled turns the feature on (default false).
	Enabled bool `yaml:"enabled"`
	// Path persists schedules across restarts (put it on the PVC).
	Path string `yaml:"path"`
	// PerUser caps schedules per user (default 5).
	PerUser int `yaml:"perUser"`
	// Total caps schedules across all users (default 200).
	Total int `yaml:"total"`
	// MinInterval is the shortest cadence a user may ask for (default 1h).
	MinInterval string `yaml:"minInterval"`
	// Concurrency caps simultaneous scheduled runs so they never crowd out
	// interactive users (default 2).
	Concurrency int `yaml:"concurrency"`
	// Timeout bounds one scheduled run (default 5m).
	Timeout string `yaml:"timeout"`
}

// API is the optional HTTP query interface: the same enforced agent loop the
// Mattermost bot uses, reachable programmatically. Callers authenticate with
// their OWN OpenShift user or ServiceAccount token (verified by mcp-authz via
// TokenReview), so they can never see more than they can with `oc`.
type API struct {
	// Enabled turns the query API on (default false).
	Enabled bool `yaml:"enabled"`
	// Addr is the listen address (default :8081). Kept separate from the
	// health/metrics port so ingress and NetworkPolicy can differ.
	Addr string `yaml:"addr"`
	// Timeout bounds one query end to end (default 5m).
	Timeout string `yaml:"timeout"`
}

// Limits guards the bot against abuse from clients (rate + input size).
type Limits struct {
	// RatePerMin caps messages/minute per user (0 = unlimited). Default 20.
	RatePerMin int `yaml:"ratePerMin"`
	// RateBurst is the allowed short burst (default = RatePerMin).
	RateBurst int `yaml:"rateBurst"`
	// MaxQueryRunes rejects overly long messages (default 4000).
	MaxQueryRunes int `yaml:"maxQueryRunes"`
}

// Agent configures the in-bot MCP tool-calling loop: an
// Anthropic-style reasoning model drives the per-cluster MCP servers, and the
// bot enforces namespace scope on every tool result.
type Agent struct {
	LLM           LLM `yaml:"llm"`
	MaxIterations int `yaml:"maxIterations"` // default 8
	// Persona is the bot's identity + greeting/help behavior (SnappCloud default).
	Persona      string `yaml:"persona"`
	SystemPrompt string `yaml:"systemPrompt"` // optional; overrides the built-in default
	// FallbackLLM is an optional backup model. When the primary fails
	// repeatedly the bot serves from this one and returns to the primary on its
	// own. Empty = no failover.
	FallbackLLM FallbackLLM `yaml:"fallbackLLM"`
	// ToolGuidance is MCP tool-usage guidance ("skills") appended to every prompt.
	ToolGuidance string         `yaml:"toolGuidance"`
	Clusters     []AgentCluster `yaml:"clusters"`
	// GlobalServers are namespace-agnostic MCP servers (e.g. general docs)
	// available to every authorized user, not tied to a cluster or scope-filtered.
	GlobalServers []MCPServer         `yaml:"globalServers"`
	ToolRules     map[string]ToolRule `yaml:"toolRules"` // per-tool namespace-arg overrides
}

// LLM points at an Anthropic-style Messages endpoint (e.g. llm.snapp.tech).
type LLM struct {
	BaseURL   string `yaml:"baseURL"`
	APIKeyEnv string `yaml:"apiKeyEnv"`
	Model     string `yaml:"model"`
	MaxTokens int    `yaml:"maxTokens"` // default 8192
	Version   string `yaml:"version"`   // anthropic-version, default 2023-06-01
	Timeout   string `yaml:"timeout"`   // default 10m
}

// FallbackLLM configures the backup model and the failover circuit breaker.
// Only Model is required — everything else inherits from agent.llm, so a second
// model on the same endpoint needs a single line.
type FallbackLLM struct {
	Model string `yaml:"model"` // empty = failover disabled
	// Optional overrides; default to the primary's values.
	BaseURL   string `yaml:"baseURL"`
	APIKeyEnv string `yaml:"apiKeyEnv"`
	MaxTokens int    `yaml:"maxTokens"`
	Version   string `yaml:"version"`
	Timeout   string `yaml:"timeout"`
	// FailureThreshold is how many consecutive primary failures switch traffic
	// to the backup (default 3).
	FailureThreshold int `yaml:"failureThreshold"`
	// CooldownPeriod is how long the backup serves before the primary is probed
	// again (default 5m).
	CooldownPeriod string `yaml:"cooldownPeriod"`
}

// AgentCluster is one cluster's MCP servers. Name MUST match the mcp-authz
// region name (scope key). Alias is a short tool-name prefix.
type AgentCluster struct {
	Name    string      `yaml:"name"`
	Alias   string      `yaml:"alias"`
	Servers []MCPServer `yaml:"servers"`
}

// MCPServer is one MCP endpoint. AuthHeaderEnv names an env var holding the full
// Authorization header value (e.g. "Basic ...") — empty for no auth. Name is an
// optional label.
type MCPServer struct {
	Name          string `yaml:"name"`
	URL           string `yaml:"url"`
	AuthHeaderEnv string `yaml:"authHeaderEnv"`
	// Alias groups a globalServer's tools under a tool-name tag (default "docs").
	// Global servers sharing an alias are merged; distinct aliases are exposed as
	// separate [alias] tool groups. Ignored for per-cluster servers.
	Alias string `yaml:"alias"`
	// SelfAuthorized marks a trusted, identity-aware server (e.g. argocd-mcp): the
	// caller's identity is forwarded as the X-Remote-User header AND all of the
	// server's tools authorize the caller themselves, so the bot skips its
	// namespace enforcement/filtering and returns their results unfiltered
	// (trusting the server the same way it trusts mcp-authz). Off by default.
	SelfAuthorized bool `yaml:"selfAuthorized"`
}

// ToolRule overrides where a tool's namespace(s) live (default: arg "namespace",
// plain). Format is "plain" or "slash" (namespace/name).
type ToolRule struct {
	NamespaceArgs    []string `yaml:"namespaceArgs"`
	Format           string   `yaml:"format"`
	RequireNamespace bool     `yaml:"requireNamespace"`
	// ClusterAdminOnly gates a cluster-infrastructure tool (nodes, BGP, agent
	// status) to callers with cluster-wide access; their results are returned
	// unfiltered.
	ClusterAdminOnly bool `yaml:"clusterAdminOnly"`
}

// Mattermost configures the bot's Mattermost connection.
type Mattermost struct {
	URL         string            `yaml:"url"`
	TokenEnv    string            `yaml:"tokenEnv"`
	IdentityMap map[string]string `yaml:"identityMap"`
	// RequireMention: answer channel messages only when @-mentioned (DMs always
	// answered). Default true.
	RequireMention *bool `yaml:"requireMention"`
}

func (m Mattermost) requireMention() bool { return m.RequireMention == nil || *m.RequireMention }

// Memory configures per-thread conversation memory.
type Memory struct {
	// ConversationTTL keeps a thread/DM's memory alive for this long after its
	// last message (default 1h). "0" disables memory.
	ConversationTTL string `yaml:"conversationTTL"`
	// MemoryPath persists the per-thread transcript to this file so users can
	// continue past conversations across bot restarts. Empty = in-memory only.
	// Put it on a PVC.
	MemoryPath string `yaml:"memoryPath"`
}

// Authz configures how the bot reaches the per-region mcp-authz APIs.
type Authz struct {
	// TokenEnv names the env var holding the bearer token presented to every
	// mcp-authz instance (shared secret).
	TokenEnv string `yaml:"tokenEnv"`
	// CacheTTL caches each user's aggregated scope for this long (default 5m).
	CacheTTL string `yaml:"cacheTTL"`
	// Timeout per region call (default 10s).
	Timeout string `yaml:"timeout"`
	// Regions are the mcp-authz endpoints, one per cluster. Region name is the
	// contract with agent.clusters[].name (per-cluster MCP tool group).
	Regions []Region `yaml:"regions"`
}

// Region is one cluster's mcp-authz endpoint.
type Region struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
}

// Load reads, parses, defaults, and validates a config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Agent.MaxIterations <= 0 {
		c.Agent.MaxIterations = 8
	}
	if c.Agent.LLM.APIKeyEnv == "" {
		c.Agent.LLM.APIKeyEnv = "LLM_API_KEY"
	}
	if c.Agent.LLM.MaxTokens <= 0 {
		c.Agent.LLM.MaxTokens = 8192
	}
	if c.Mattermost.TokenEnv == "" {
		c.Mattermost.TokenEnv = "MATTERMOST_TOKEN"
	}
	if c.Memory.ConversationTTL == "" {
		c.Memory.ConversationTTL = "1h"
	}
	if c.Authz.TokenEnv == "" {
		c.Authz.TokenEnv = "MCP_AUTHZ_TOKEN"
	}
	if c.Authz.CacheTTL == "" {
		c.Authz.CacheTTL = "5m"
	}
	if c.Authz.Timeout == "" {
		c.Authz.Timeout = "30s"
	}
	if c.Limits.RatePerMin == 0 {
		c.Limits.RatePerMin = 20
	}
	if c.API.Addr == "" {
		c.API.Addr = ":8081"
	}
}

func (c *Config) validate() error {
	if strings.TrimSpace(c.Mattermost.URL) == "" {
		return fmt.Errorf("mattermost.url is required")
	}
	if strings.TrimSpace(c.Agent.LLM.BaseURL) == "" || strings.TrimSpace(c.Agent.LLM.Model) == "" {
		return fmt.Errorf("agent.llm.baseURL and agent.llm.model are required")
	}
	if len(c.Agent.Clusters) == 0 {
		return fmt.Errorf("agent.clusters must list at least one cluster with MCP servers")
	}
	if len(c.Authz.Regions) == 0 {
		return fmt.Errorf("authz.regions must list at least one mcp-authz endpoint")
	}
	seen := map[string]bool{}
	for i, r := range c.Authz.Regions {
		if strings.TrimSpace(r.Name) == "" || strings.TrimSpace(r.URL) == "" {
			return fmt.Errorf("authz.regions[%d]: name and url are required", i)
		}
		if seen[r.Name] {
			return fmt.Errorf("authz.regions: duplicate region name %q", r.Name)
		}
		seen[r.Name] = true
	}
	return nil
}

// RequireMention reports whether channel messages must @-mention the bot.
func (c *Config) RequireMention() bool { return c.Mattermost.requireMention() }
