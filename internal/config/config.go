// Package config loads and validates the SnappCloud bot configuration.
//
// The bot connects to Mattermost (WebSocket) and Dify, and authorizes each
// user's query by calling the per-region mcp-authz API. It holds no cluster
// credentials — authorization lives in mcp-authz, one instance per region.
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
	Dify       Dify       `yaml:"dify"`
	Authz      Authz      `yaml:"authz"`
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

// Dify configures the workflow the bot forwards authorized queries to.
type Dify struct {
	URL       string `yaml:"url"`
	APIKeyEnv string `yaml:"apiKeyEnv"`
	// ConversationTTL keeps a thread/DM's Dify conversation (memory) alive for
	// this long after its last message (default 1h). "0" disables memory.
	ConversationTTL string `yaml:"conversationTTL"`
	// MemoryPath persists the thread/DM -> Dify-conversation mapping to this file
	// so users can continue past conversations across bot restarts. Empty =
	// in-memory only. Put it on a PVC.
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
	// contract with the Dify workflow (per-cluster MCP tool group).
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
	if c.Mattermost.TokenEnv == "" {
		c.Mattermost.TokenEnv = "MATTERMOST_TOKEN"
	}
	if c.Dify.APIKeyEnv == "" {
		c.Dify.APIKeyEnv = "DIFY_API_KEY"
	}
	if c.Dify.ConversationTTL == "" {
		c.Dify.ConversationTTL = "1h"
	}
	if c.Authz.TokenEnv == "" {
		c.Authz.TokenEnv = "MCP_AUTHZ_TOKEN"
	}
	if c.Authz.CacheTTL == "" {
		c.Authz.CacheTTL = "5m"
	}
	if c.Authz.Timeout == "" {
		c.Authz.Timeout = "10s"
	}
}

func (c *Config) validate() error {
	if strings.TrimSpace(c.Mattermost.URL) == "" {
		return fmt.Errorf("mattermost.url is required")
	}
	if strings.TrimSpace(c.Dify.URL) == "" {
		return fmt.Errorf("dify.url is required")
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
