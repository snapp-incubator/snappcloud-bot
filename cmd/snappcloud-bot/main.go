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
	"github.com/snapp-incubator/snappcloud-bot/internal/api"
	"github.com/snapp-incubator/snappcloud-bot/internal/authzclient"
	"github.com/snapp-incubator/snappcloud-bot/internal/bot"
	"github.com/snapp-incubator/snappcloud-bot/internal/brain"
	"github.com/snapp-incubator/snappcloud-bot/internal/config"
	"github.com/snapp-incubator/snappcloud-bot/internal/llm"
	"github.com/snapp-incubator/snappcloud-bot/internal/mattermost"
	"github.com/snapp-incubator/snappcloud-bot/internal/metrics"
	"github.com/snapp-incubator/snappcloud-bot/internal/schedule"
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
	clusterNames := make([]string, 0, len(cfg.Agent.Clusters))
	for _, c := range cfg.Agent.Clusters {
		clusterNames = append(clusterNames, c.Name)
	}
	// Create the metric series up front so dashboards show 0 rather than "No
	// data" before the first message.
	metrics.Init(clusterNames, names)

	authzBase := authzclient.New(regions, authzToken, timeout, log)
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

	// User-defined recurring queries. The store is created before the service
	// because the service implements the runner's Answerer.
	var schedStore *schedule.Store
	if cfg.Schedules.Enabled {
		// Zero means "use the store's default", which is where the real defaults
		// live — don't duplicate them here.
		var minInterval time.Duration
		if cfg.Schedules.MinInterval != "" {
			d, err := time.ParseDuration(cfg.Schedules.MinInterval)
			if err != nil {
				return fmt.Errorf("parse schedules.minInterval: %w", err)
			}
			minInterval = d
		}
		schedStore = schedule.NewStore(cfg.Schedules.Path, schedule.Limits{
			PerUser:     cfg.Schedules.PerUser,
			Total:       cfg.Schedules.Total,
			MinInterval: minInterval,
		})
		total, owners := schedStore.Stats()
		metrics.Schedules.Set(float64(total))
		metrics.ScheduleOwners.Set(float64(owners))
		metrics.ScheduleLimit.Set(float64(schedStore.Limits().Total))
	}

	// One limiter shared by Mattermost and the HTTP API: a caller cannot bypass
	// their budget by switching entrypoint.
	limiter := bot.NewRateLimiter(cfg.Limits.RatePerMin, cfg.Limits.RateBurst)

	svc := bot.New(mm, theBrain, resolver, bot.Options{
		ConversationTTL: convTTL,
		MemoryPath:      cfg.Memory.MemoryPath,
		IdentityMap:     cfg.Mattermost.IdentityMap,
		BotUsername:     me.Username,
		RequireMention:  cfg.RequireMention(),
		RatePerMin:      cfg.Limits.RatePerMin,
		RateBurst:       cfg.Limits.RateBurst,
		MaxQueryRunes:   cfg.Limits.MaxQueryRunes,
		Limiter:         limiter,
		Schedules:       schedStore,
	}, log)
	go svc.StartSweeper(ctx)

	if schedStore != nil {
		schedTimeout := 5 * time.Minute
		if cfg.Schedules.Timeout != "" {
			d, err := time.ParseDuration(cfg.Schedules.Timeout)
			if err != nil {
				return fmt.Errorf("parse schedules.timeout: %w", err)
			}
			schedTimeout = d
		}
		runner := schedule.NewRunner(schedStore, svc, schedule.RunnerOptions{
			Concurrency: cfg.Schedules.Concurrency,
			Timeout:     schedTimeout,
		}, log)
		go runner.Start(ctx)
		log.Info("schedules enabled", "stored", schedStore.Count(),
			"perUser", schedStore.Limits().PerUser, "total", schedStore.Limits().Total,
			"minInterval", schedStore.Limits().MinInterval)
	}

	go serveHealth(ctx, addr, log)

	if cfg.API.Enabled {
		apiTimeout := 5 * time.Minute
		if cfg.API.Timeout != "" {
			d, err := time.ParseDuration(cfg.API.Timeout)
			if err != nil {
				return fmt.Errorf("parse api.timeout: %w", err)
			}
			apiTimeout = d
		}
		h := api.New(theBrain, authzBase, limiter, api.Options{
			MaxQueryRunes: cfg.Limits.MaxQueryRunes,
			Timeout:       apiTimeout,
		}, log)
		go serveAPI(ctx, cfg.API.Addr, h, apiTimeout, log)
		log.Info("query API enabled", "addr", cfg.API.Addr, "timeout", apiTimeout)
	}

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
			servers = append(servers, brain.Server{URL: s.URL, AuthHeader: auth, SelfAuthorized: s.SelfAuthorized})
		}
		clusters = append(clusters, brain.Cluster{Name: c.Name, Alias: c.Alias, Servers: servers})
	}

	rules := make(map[string]agent.ToolRule, len(cfg.Agent.ToolRules))
	for name, r := range cfg.Agent.ToolRules {
		f := agent.NSPlain
		if r.Format == "slash" {
			f = agent.NSSlash
		}
		rules[name] = agent.ToolRule{
			NamespaceArgs:    r.NamespaceArgs,
			Format:           f,
			RequireNamespace: r.RequireNamespace,
			ClusterAdminOnly: r.ClusterAdminOnly,
		}
	}

	globalServers := make([]brain.Server, 0, len(cfg.Agent.GlobalServers))
	for _, s := range cfg.Agent.GlobalServers {
		auth := ""
		if s.AuthHeaderEnv != "" {
			auth = os.Getenv(s.AuthHeaderEnv)
		}
		globalServers = append(globalServers, brain.Server{URL: s.URL, AuthHeader: auth, Alias: s.Alias})
	}

	// Optional backup model: anything it does not set is inherited from the
	// primary, so a second model on the same endpoint is a one-line config.
	var fallback llm.Options
	fbOpts := llm.FailoverOptions{FailureThreshold: cfg.Agent.FallbackLLM.FailureThreshold}
	if fb := cfg.Agent.FallbackLLM; strings.TrimSpace(fb.Model) != "" {
		fbKey := llmKey
		if fb.APIKeyEnv != "" && fb.APIKeyEnv != cfg.Agent.LLM.APIKeyEnv {
			if v := os.Getenv(fb.APIKeyEnv); v != "" {
				fbKey = v
			} else {
				return nil, fmt.Errorf("fallback llm api key env %q is empty", fb.APIKeyEnv)
			}
		}
		fbTimeout := llmTimeout
		if fb.Timeout != "" {
			d, err := time.ParseDuration(fb.Timeout)
			if err != nil {
				return nil, fmt.Errorf("parse agent.fallbackLLM.timeout: %w", err)
			}
			fbTimeout = d
		}
		if fb.CooldownPeriod != "" {
			d, err := time.ParseDuration(fb.CooldownPeriod)
			if err != nil {
				return nil, fmt.Errorf("parse agent.fallbackLLM.cooldownPeriod: %w", err)
			}
			fbOpts.CooldownPeriod = d
		}
		fallback = llm.Options{
			BaseURL:   firstNonEmpty(fb.BaseURL, cfg.Agent.LLM.BaseURL),
			APIKey:    fbKey,
			Model:     fb.Model,
			MaxTokens: firstPositive(fb.MaxTokens, cfg.Agent.LLM.MaxTokens),
			Version:   firstNonEmpty(fb.Version, cfg.Agent.LLM.Version),
			Timeout:   fbTimeout,
		}
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
		FallbackLLM:   fallback,
		FailoverOpts:  fbOpts,
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

// firstNonEmpty returns a if set, else b.
func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// firstPositive returns a if > 0, else b.
func firstPositive(a, b int) int {
	if a > 0 {
		return a
	}
	return b
}

// serveAPI runs the HTTP query interface until ctx is cancelled.
func serveAPI(ctx context.Context, addr string, h *api.Handler, timeout time.Duration, log *slog.Logger) {
	mux := http.NewServeMux()
	h.Routes(mux)
	hs := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		// A query runs the whole agent loop, so the write deadline must exceed it.
		WriteTimeout: timeout + time.Minute,
		IdleTimeout:  120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		sctx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_ = hs.Shutdown(sctx)
	}()
	if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("query API server", "err", err)
	}
}

func serveHealth(ctx context.Context, addr string, log *slog.Logger) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.Handle("GET /metrics", metrics.Handler())
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
