package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"zenbridge/internal/catalog"
	"zenbridge/internal/config"
	"zenbridge/internal/server"
)

var version = "dev"
var commit = "unknown"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "zen-bridge:", err)
		os.Exit(1)
	}
}
func run() error {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
		args = args[1:]
	}
	switch cmd {
	case "version", "--version":
		fmt.Printf("zen-bridge %s (%s)\n", version, commit)
		return nil
	case "accounts":
		if len(args) == 0 || args[0] != "list" {
			return fmt.Errorf("usage: accounts list --config FILE")
		}
		fs := flag.NewFlagSet("accounts list", flag.ContinueOnError)
		path := fs.String("config", "config.json", "configuration file")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		cfg, err := config.LoadConfig(*path)
		if err != nil {
			return err
		}
		accounts, err := config.LoadConfiguredAccounts(cfg)
		if err != nil {
			return err
		}
		type accountInfo struct {
			Index      int    `json:"index"`
			ID         string `json:"id"`
			Enabled    bool   `json:"enabled"`
			ModelCount int    `json:"model_count"`
		}
		out := make([]accountInfo, 0, len(accounts))
		for i, a := range accounts {
			out = append(out, accountInfo{Index: i + 1, ID: a.ID, Enabled: a.Enabled, ModelCount: len(a.Models)})
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	case "models":
		if len(args) == 0 {
			return fmt.Errorf("usage: models discover|check --account ID --config FILE")
		}
		op := args[0]
		args = args[1:]
		if op != "discover" && op != "check" {
			return fmt.Errorf("models operation must be discover or check")
		}
		fs := flag.NewFlagSet("models "+op, flag.ContinueOnError)
		path := fs.String("config", "config.json", "configuration file")
		account := fs.String("account", "", "configured account ID")
		if e := fs.Parse(args); e != nil {
			return e
		}
		if *account == "" {
			return fmt.Errorf("--account is required")
		}
		cfg, e := config.LoadConfig(*path)
		if e != nil {
			return e
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		var report catalog.Report
		if op == "discover" {
			report, e = catalog.DiscoverModels(ctx, cfg, *account)
		} else {
			fmt.Fprintln(os.Stderr, "Checking free models sequentially; may consume upstream quota. No automatic retries.")
			report, e = catalog.CheckModels(ctx, cfg, *account)
		}
		if e != nil {
			return e
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	case "token":
		return fmt.Errorf("the token command was removed; set your own access key in config.json as \"client_token\"")
	case "healthcheck":
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		url := fs.String("url", "http://127.0.0.1:18788/healthz", "local health endpoint")
		if e := fs.Parse(args); e != nil {
			return e
		}
		c := &http.Client{Timeout: 3 * time.Second}
		r, e := c.Get(*url)
		if e != nil {
			return fmt.Errorf("healthcheck failed")
		}
		defer r.Body.Close()
		if r.StatusCode != 200 {
			return fmt.Errorf("healthcheck status %d", r.StatusCode)
		}
		return nil
	case "check", "serve":
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		path := fs.String("config", "config.json", "configuration file")
		if e := fs.Parse(args); e != nil {
			return e
		}
		cfg, e := config.LoadConfig(*path)
		if e != nil {
			return e
		}
		accounts, e := config.LoadConfiguredAccounts(cfg)
		if e != nil {
			return e
		}
		if cmd == "check" {
			fmt.Println("configuration valid (no upstream requests made)")
			return nil
		}
		token, e := config.ClientToken(cfg)
		if e != nil {
			return e
		}
		level := slog.LevelInfo
		switch cfg.LogLevel {
		case "debug":
			level = slog.LevelDebug
		case "warn":
			level = slog.LevelWarn
		case "error":
			level = slog.LevelError
		}
		logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
		app := server.NewServer(cfg, accounts, token, logger)
		defer app.Close()
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		go app.WatchAccounts(ctx)
		go app.RunAutoCatalog(ctx)
		hup := make(chan os.Signal, 1)
		signal.Notify(hup, syscall.SIGHUP)
		defer signal.Stop(hup)
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-hup:
					if e := app.ReloadAccounts(); e != nil {
						logger.Warn("accounts reload rejected; previous configuration retained")
					}
				}
			}
		}()
		srv := &http.Server{Addr: cfg.Listen, Handler: app.Handler()}
		done := make(chan error, 1)
		go func() { done <- srv.ListenAndServe() }()
		logger.Info("bridge started; local access key comes from config.json client_token", "listen", cfg.Listen, "version", version)
		select {
		case e := <-done:
			if e != nil && e != http.ErrServerClosed {
				return e
			}
			return nil
		case <-ctx.Done():
			deadline, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if e := srv.Shutdown(deadline); e != nil {
				_ = srv.Close()
			}
			return nil
		}
	default:
		return fmt.Errorf("commands: serve, check, accounts list, models discover|check, healthcheck, version")
	}
}
