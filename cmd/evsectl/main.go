// Command evsectl controls a BTC Power dual-port EVSE: it talks to the
// charge control and power meter boards over serial and serves a status UI
// and JSON API.
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

	"github.com/ArrestedLightning/btcpower_evse_controller/internal/app"
	"github.com/ArrestedLightning/btcpower_evse_controller/internal/config"
	"github.com/ArrestedLightning/btcpower_evse_controller/internal/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "probe" {
		os.Exit(probeMain(os.Args[2:]))
	}
	os.Exit(run())
}

func run() int {
	configPath := flag.String("config", "/etc/evse-controller/config.yaml", "configuration file")
	simulate := flag.Bool("simulate", false, "use simulated chargers and meters instead of serial ports")
	dataDir := flag.String("data-dir", "", "override data_dir from the config file")
	listen := flag.String("listen", "", "override listen address from the config file")
	debugSerial := flag.Bool("debug-serial", false, "log all serial traffic (implies -log-level debug)")
	logLevel := flag.String("log-level", "info", "log level: debug, info, warn, error")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage:\n  %s [flags]\n  %s probe -h\n\nFlags:\n", os.Args[0], os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return 0
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintln(os.Stderr, "invalid -log-level:", err)
		return 2
	}
	if *debugSerial {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Error("loading configuration", "err", err)
		return 1
	}
	if *dataDir != "" {
		cfg.DataDir = *dataDir
	}
	if *listen != "" {
		cfg.Listen = *listen
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := app.New(*configPath, cfg, *simulate, *debugSerial, log)
	if err != nil {
		log.Error("opening data directory", "dir", cfg.DataDir, "err", err)
		return 1
	}
	a.Start(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           (&web.Server{App: a, Version: version, Log: log}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("started", "version", version, "listen", cfg.Listen, "chargers", len(cfg.Chargers),
		"meters", len(cfg.Meters), "simulate", *simulate)

	code := 0
	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("web server", "err", err)
			code = 1
		}
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shutCtx)
	stop()
	a.Stop()
	return code
}
