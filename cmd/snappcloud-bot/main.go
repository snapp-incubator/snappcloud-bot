// Command snappcloud-bot is the SnappCloud Mattermost bot. It listens on the
// Mattermost WebSocket, resolves the user's SSO identity, authorizes the query
// by calling the per-region mcp-authz APIs (it holds no cluster credentials),
// and forwards authorized queries to the Dify workflow.
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

	"github.com/snapp-incubator/snappcloud-bot/internal/authzclient"
	"github.com/snapp-incubator/snappcloud-bot/internal/bot"
	"github.com/snapp-incubator/snappcloud-bot/internal/config"
	"github.com/snapp-incubator/snappcloud-bot/internal/dify"
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
	difyKey := os.Getenv(cfg.Dify.APIKeyEnv)
	if difyKey == "" {
		return fmt.Errorf("dify api key env %q is empty", cfg.Dify.APIKeyEnv)
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
	convTTL, err := time.ParseDuration(cfg.Dify.ConversationTTL)
	if err != nil {
		return fmt.Errorf("parse dify.conversationTTL: %w", err)
	}
	scopeTokenTTL, err := time.ParseDuration(cfg.Authz.ScopeTokenTTL)
	if err != nil {
		return fmt.Errorf("parse authz.scopeTokenTTL: %w", err)
	}
	var scopeSecret string
	if cfg.Authz.ScopeSecretEnv != "" {
		scopeSecret = os.Getenv(cfg.Authz.ScopeSecretEnv)
		if scopeSecret == "" {
			return fmt.Errorf("scope secret env %q is empty", cfg.Authz.ScopeSecretEnv)
		}
	}

	regions := make([]authzclient.Region, 0, len(cfg.Authz.Regions))
	names := make([]string, 0, len(cfg.Authz.Regions))
	for _, r := range cfg.Authz.Regions {
		regions = append(regions, authzclient.Region{Name: r.Name, URL: r.URL})
		names = append(names, r.Name)
	}
	resolver := authzclient.NewCachedResolver(authzclient.New(regions, authzToken, timeout), ttl)
	log.Info("authz ready", "regions", names, "cacheTTL", ttl)

	mm := mattermost.NewClient(cfg.Mattermost.URL, mmToken)
	difyClient := dify.NewClient(cfg.Dify.URL, difyKey)

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

	svc := bot.New(mm, difyClient, resolver, bot.Options{
		ConversationTTL: convTTL,
		IdentityMap:     cfg.Mattermost.IdentityMap,
		BotUsername:     me.Username,
		RequireMention:  cfg.RequireMention(),
		ScopeSecret:     scopeSecret,
		ScopeTokenTTL:   scopeTokenTTL,
	}, log)
	go svc.StartSweeper(ctx)
	if scopeSecret != "" {
		log.Info("mcp-gateway scope enforcement enabled", "tokenTTL", scopeTokenTTL)
	}

	go serveHealth(ctx, addr, log)

	log.Info("starting SnappCloud bot",
		"version", version.Version, "mattermost", cfg.Mattermost.URL, "dify", cfg.Dify.URL)
	mm.Listen(ctx, me.ID, svc.OnPost, log)
	log.Info("shut down")
	return nil
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
