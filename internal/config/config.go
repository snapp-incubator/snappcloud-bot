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
}

// Agent configures the in-bot MCP tool-calling loop: an
// Anthropic-style reasoning model drives the per-cluster MCP servers, and the
// bot enforces namespace scope on every tool result.
type Agent struct {
	LLM           LLM                 `yaml:"llm"`
	MaxIterations int                 `yaml:"maxIterations"` // default 8
	SystemPrompt  string              `yaml:"systemPrompt"`  // optional; built-in default
	Clusters      []AgentCluster      `yaml:"clusters"`
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

// AgentCluster is one cluster's MCP servers. Name MUST match the mcp-authz
// region name (scope key). Alias is a short tool-name prefix.
type AgentCluster struct {
	Name    string      `yaml:"name"`
	Alias   string      `yaml:"alias"`
	Servers []MCPServer `yaml:"servers"`
}

// MCPServer is one MCP endpoint. AuthHeaderEnv names an env var holding the full
// Authorization header value (e.g. "Basic ...") — empty for no auth.
type MCPServer struct {
	Name          string `yaml:"name"`
	URL           string `yaml:"url"`
	AuthHeaderEnv string `yaml:"authHeaderEnv"`
}

// ToolRule overrides where a tool's namespace(s) live (default: arg "namespace",
// plain). Format is "plain" or "slash" (namespace/name).
type ToolRule struct {
	NamespaceArgs    []string `yaml:"namespaceArgs"`
	Format           string   `yaml:"format"`
	RequireNamespace bool     `yaml:"requireNamespace"`
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
