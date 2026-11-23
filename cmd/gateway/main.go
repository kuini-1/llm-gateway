// Command gateway runs the LLM gateway, or creates projects and API keys.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/kuini-1/llm-gateway/assets"
	"github.com/kuini-1/llm-gateway/internal/api"
	"github.com/kuini-1/llm-gateway/internal/budget"
	"github.com/kuini-1/llm-gateway/internal/cache"
	"github.com/kuini-1/llm-gateway/internal/config"
	"github.com/kuini-1/llm-gateway/internal/pricing"
	"github.com/kuini-1/llm-gateway/internal/router"
	"github.com/kuini-1/llm-gateway/internal/store"
)

const usage = `gateway: one API in front of Claude, OpenAI, DeepSeek and self-hosted models

Usage:
  gateway serve   [-config gateway.yaml]
  gateway project -name NAME [-budget USD]            create a project
  gateway key     -project ID -name NAME [-budget USD] [-rpm N]   create an API key (printed once)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(ctx, os.Args[2:])
	case "project":
		err = createProject(ctx, os.Args[2:])
	case "key":
		err = createKey(ctx, os.Args[2:])
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func configPath(fs *flag.FlagSet) *string {
	def := os.Getenv("GATEWAY_CONFIG")
	if def == "" {
		def = "gateway.yaml"
	}
	return fs.String("config", def, "config file")
}

func open(ctx context.Context, cfg *config.Config) (*store.Store, *redis.Client, error) {
	st, err := store.Open(ctx, cfg.PostgresURL)
	if err != nil {
		return nil, nil, err
	}
	opt, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		st.Close()
		return nil, nil, fmt.Errorf("redis_url: %w", err)
	}
	rdb := redis.NewClient(opt)
	if err := rdb.Ping(ctx).Err(); err != nil {
		st.Close()
		return nil, nil, fmt.Errorf("redis: %w", err)
	}
	return st, rdb, nil
}

func serve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	path := configPath(fs)
	fs.Parse(args)
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	st, rdb, err := open(ctx, cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	defer rdb.Close()

	reqLog := store.NewLogger(st)
	reqLog.OnErr = func(err error) { log.Error("request log", "err", err) }
	defer reqLog.Close()

	g := &api.Gateway{
		Cfg: cfg, Router: router.New(cfg), Prices: pricing.Table(cfg.Prices), Store: st, ReqLog: reqLog,
		Budget:     &budget.Budget{RDB: rdb, Load: st.MonthSpend},
		Limiter:    &budget.Limiter{RDB: rdb},
		AdminToken: os.Getenv("GATEWAY_ADMIN_TOKEN"),
		Dashboard:  assets.Dashboard(),
		OpenAPI:    assets.OpenAPI(),
		Log:        log,
		Ready: func(ctx context.Context) error {
			if err := st.DB.Ping(ctx); err != nil {
				return fmt.Errorf("postgres: %w", err)
			}
			return rdb.Ping(ctx).Err()
		},
	}
	if cfg.EmbeddingCache.Enabled {
		g.EmbCache = &cache.Embeddings{RDB: rdb, TTL: cfg.EmbeddingCache.TTL}
	}
	if g.AdminToken == "" {
		log.Warn("GATEWAY_ADMIN_TOKEN is not set: the admin API and dashboard are disabled")
	}

	srv := &http.Server{Addr: cfg.Listen, Handler: g.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	log.Info("gateway listening", "addr", cfg.Listen, "routes", len(cfg.Routes), "providers", len(cfg.Providers))
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func createProject(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("project", flag.ExitOnError)
	path := configPath(fs)
	name := fs.String("name", "", "project name")
	budgetUSD := fs.Float64("budget", 0, "monthly budget in USD (0 = no cap)")
	fs.Parse(args)
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.PostgresURL)
	if err != nil {
		return err
	}
	defer st.Close()
	p, err := st.CreateProject(ctx, *name, *budgetUSD)
	if err != nil {
		return err
	}
	fmt.Printf("project %s (%s), budget $%.2f/month\n", p.ID, p.Name, p.BudgetUSD)
	return nil
}

func createKey(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("key", flag.ExitOnError)
	path := configPath(fs)
	project := fs.String("project", "", "project id")
	name := fs.String("name", "", "who the key is for")
	budgetUSD := fs.Float64("budget", 0, "monthly budget in USD for this key (0 = no cap)")
	rpm := fs.Int("rpm", 0, "requests per minute (0 = default)")
	fs.Parse(args)
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.PostgresURL)
	if err != nil {
		return err
	}
	defer st.Close()
	k, secret, err := st.CreateKey(ctx, *project, *name, *budgetUSD, *rpm)
	if err != nil {
		return err
	}
	fmt.Printf("key %s for %s\n%s\n(store it now; it is not shown again)\n", k.ID, k.Name, secret)
	return nil
}
