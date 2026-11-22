// Command vigilant is a lightweight NVR: go2rtc-backed restreaming, hourly
// continuous recording, motion detection with sliding-window events,
// dual retention policies, a REST API, and an embedded web UI.
package main

import (
	"context"
	"embed"
	"flag"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jbutlerdev/vigilant/internal/api"
	"github.com/jbutlerdev/vigilant/internal/app"
	"github.com/jbutlerdev/vigilant/internal/config"
)

//go:embed web
var webEmbed embed.FS

var version = "dev"

func main() {
	configPath := flag.String("config", "config.yaml", "path to YAML config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		os.Stdout.WriteString("vigilant " + version + "\n")
		return
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if err := run(*configPath, log); err != nil {
		log.Error("vigilant exited with error", "err", err)
		os.Exit(1)
	}
}

func run(configPath string, log *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	log.Info("vigilant starting", "version", version, "cameras", len(cfg.Cameras), "data_dir", cfg.DataDir)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := app.New(cfg, log)
	if err != nil {
		return err
	}
	if err := a.Start(ctx); err != nil {
		return err
	}
	defer a.Stop()

	go a.Store.RunCleaner(ctx)

	webFS, err := fs.Sub(webEmbed, "web")
	if err != nil {
		return err
	}
	srv := api.New(a, log, webFS)
	httpSrv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("http listening", "addr", cfg.Server.Listen)
		errCh <- httpSrv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		log.Info("shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
		return nil
	case err := <-errCh:
		return err
	}
}
