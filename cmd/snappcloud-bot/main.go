// Command snappcloud-bot is the SnappCloud Mattermost bot. It listens on the
// Mattermost WebSocket, resolves the user's SSO identity, authorizes the query
// via the per-region mcp-authz APIs (it holds no cluster credentials), and runs
// the in-bot MCP agent that drives the per-cluster MCP servers under namespace
// enforcement.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/snapp-incubator/snappcloud-bot/internal/agent"
	"github.com/snapp-incubator/snappcloud-bot/internal/authzclient"
	"github.com/snapp-incubator/snappcloud-bot/internal/bot"
	"github.com/snapp-incubator/snappcloud-bot/internal/brain"
	"github.com/snapp-incubator/snappcloud-bot/internal/config"
	"github.com/snapp-incubator/snappcloud-bot/internal/llm"
	"github.com/snapp-incubator/snappcloud-bot/internal/mattermost"
	"github.com/snapp-incubator/snappcloud-bot/internal/version"
)

func main() {
	var (
		configPath  string
		addr        string
		logLevel    string
		showVersion bool
	)
	flag.StringVar(&configPath, "config", "/etc/snappcloud-bot/config.yaml", "Path to config file")
	flag.StringVar(&addr, "addr", ":8080", "Health/readiness HTTP listen address")
	flag.StringVar(&logLevel, "log-level", "info", "Log level: debug, info, warn, error")
	flag.BoolVar(&showVersion, "version", false, "Print version and exit")
	flag.Parse()

	if showVersion {
		fmt.Println(version.String())
		return
	}

	log := newLogger(logLevel)
	if err := run(configPath, addr, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(configPath, addr string, log *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	mmToken := os.Getenv(cfg.Mattermost.TokenEnv)
	if mmToken == "" {
		return fmt.Errorf("mattermost token env %q is empty", cfg.Mattermost.TokenEnv)
	}
	llmKey := os.Getenv(cfg.Agent.LLM.APIKeyEnv)
	if llmKey == "" {
		return fmt.Errorf("llm api key env %q is empty", cfg.Agent.LLM.APIKeyEnv)
	}
	authzToken := os.Getenv(cfg.Authz.TokenEnv)

	timeout, err := time.ParseDuration(cfg.Authz.Timeout)
	if err != nil {
		return fmt.Errorf("parse authz.timeout: %w", err)
	}
	ttl, err := time.ParseDuration(cfg.Authz.CacheTTL)
	if err != nil {
		return fmt.Errorf("parse authz.cacheTTL: %w", err)
	}
	convTTL, err := time.ParseDuration(cfg.Memory.ConversationTTL)
	if err != nil {
		return fmt.Errorf("parse memory.conversationTTL: %w", err)
	}

	regions := make([]authzclient.Region, 0, len(cfg.Authz.Regions))
	names := make([]string, 0, len(cfg.Authz.Regions))
	for _, r := range cfg.Authz.Regions {
		regions = append(regions, authzclient.Region{Name: r.Name, URL: r.URL})
		names = append(names, r.Name)
	}
	authzBase := authzclient.New(regions, authzToken, timeout)
	resolver := authzclient.NewCachedResolver(authzBase, ttl)
	log.Info("authz ready", "regions", names, "cacheTTL", ttl)

	theBrain, err := buildBrain(cfg, llmKey, authzBase, log)
	if err != nil {
		return err
	}

	mm := mattermost.NewClient(cfg.Mattermost.URL, mmToken)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if cr, ok := resolver.(*authzclient.CachedResolver); ok {
		go cr.StartSweeper(ctx)
	}

	me, err := mm.Me(ctx)
	if err != nil {
		return fmt.Errorf("mattermost auth (check token/url): %w", err)
	}
	log.Info("connected to mattermost", "bot", me.Username, "id", me.ID)

	svc := bot.New(mm, theBrain, resolver, bot.Options{
		ConversationTTL: convTTL,
		MemoryPath:      cfg.Memory.MemoryPath,
		IdentityMap:     cfg.Mattermost.IdentityMap,
		BotUsername:     me.Username,
		RequireMention:  cfg.RequireMention(),
	}, log)
	go svc.StartSweeper(ctx)

	go serveHealth(ctx, addr, log)

	log.Info("starting SnappCloud bot",
		"version", version.Version, "mattermost", cfg.Mattermost.URL)
	mm.Listen(ctx, me.ID, svc.OnPost, log)
	log.Info("shut down")
	return nil
}

// buildBrain constructs the in-bot agent orchestrator from config.
func buildBrain(cfg *config.Config, llmKey string, resolver agent.Resolver, log *slog.Logger) (*brain.Brain, error) {
	llmTimeout := 10 * time.Minute
	if cfg.Agent.LLM.Timeout != "" {
		d, err := time.ParseDuration(cfg.Agent.LLM.Timeout)
		if err != nil {
			return nil, fmt.Errorf("parse agent.llm.timeout: %w", err)
		}
		llmTimeout = d
	}

	clusters := make([]brain.Cluster, 0, len(cfg.Agent.Clusters))
	for _, c := range cfg.Agent.Clusters {
		servers := make([]brain.Server, 0, len(c.Servers))
		for _, s := range c.Servers {
			auth := ""
			if s.AuthHeaderEnv != "" {
				auth = os.Getenv(s.AuthHeaderEnv)
			}
			servers = append(servers, brain.Server{URL: s.URL, AuthHeader: auth})
		}
		clusters = append(clusters, brain.Cluster{Name: c.Name, Alias: c.Alias, Servers: servers})
	}

	rules := make(map[string]agent.ToolRule, len(cfg.Agent.ToolRules))
	for name, r := range cfg.Agent.ToolRules {
		f := agent.NSPlain
		if r.Format == "slash" {
			f = agent.NSSlash
		}
		rules[name] = agent.ToolRule{NamespaceArgs: r.NamespaceArgs, Format: f, RequireNamespace: r.RequireNamespace}
	}

	globalServers := make([]brain.Server, 0, len(cfg.Agent.GlobalServers))
	for _, s := range cfg.Agent.GlobalServers {
		auth := ""
		if s.AuthHeaderEnv != "" {
			auth = os.Getenv(s.AuthHeaderEnv)
		}
		globalServers = append(globalServers, brain.Server{URL: s.URL, AuthHeader: auth})
	}

	b := brain.New(brain.Options{
		LLM: llm.Options{
			BaseURL:   cfg.Agent.LLM.BaseURL,
			APIKey:    llmKey,
			Model:     cfg.Agent.LLM.Model,
			MaxTokens: cfg.Agent.LLM.MaxTokens,
			Version:   cfg.Agent.LLM.Version,
			Timeout:   llmTimeout,
		},
		MaxIter:       cfg.Agent.MaxIterations,
		Persona:       cfg.Agent.Persona,
		SystemPrompt:  cfg.Agent.SystemPrompt,
		ToolGuidance:  cfg.Agent.ToolGuidance,
		Clusters:      clusters,
		GlobalServers: globalServers,
		Rules:         rules,
		MCPTimeout:    5 * time.Minute,
		Resolver:      resolver,
	}, log)
	log.Info("agent ready", "model", cfg.Agent.LLM.Model, "clusters", len(clusters), "globalServers", len(globalServers), "maxIter", cfg.Agent.MaxIterations)
	return b, nil
}

func serveHealth(ctx context.Context, addr string, log *slog.Logger) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	hs := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = hs.Shutdown(sctx)
	}()
	if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("health server", "err", err)
	}
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv}))
}
