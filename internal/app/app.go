// Package app owns the configuration file and the running station, and
// rebuilds the station when the configuration changes.
package app

import (
	"context"
	"log/slog"
	"sync"

	"github.com/ArrestedLightning/btcpower_evse_controller/internal/config"
	"github.com/ArrestedLightning/btcpower_evse_controller/internal/station"
)

// App runs one Station at a time.
type App struct {
	ConfigPath  string
	Simulate    bool
	DebugSerial bool
	Log         *slog.Logger

	mu      sync.Mutex
	root    context.Context
	cfg     *config.Config
	store   *station.Store
	st      *station.Station
	cancel  context.CancelFunc
	stopped chan struct{}
}

// New creates an App with a loaded configuration.
func New(path string, cfg *config.Config, simulate, debug bool, log *slog.Logger) (*App, error) {
	store, err := station.OpenStore(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	return &App{ConfigPath: path, Simulate: simulate, DebugSerial: debug, Log: log, cfg: cfg, store: store}, nil
}

// Start launches the station.
func (a *App) Start(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.root = ctx
	a.startLocked()
}

func (a *App) startLocked() {
	st := station.New(a.cfg, station.Options{Simulate: a.Simulate, DebugSerial: a.DebugSerial, Log: a.Log, Store: a.store})
	sctx, cancel := context.WithCancel(a.root)
	done := make(chan struct{})
	go func() { st.Run(sctx); close(done) }()
	a.st, a.cancel, a.stopped = st, cancel, done
}

func (a *App) stopLocked() {
	if a.cancel != nil {
		a.cancel()
		<-a.stopped
	}
}

// Stop shuts the station down and waits for serial ports to close.
func (a *App) Stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stopLocked()
}

// Station returns the running station.
func (a *App) Station() *station.Station {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.st
}

// Config returns a copy of the current configuration.
func (a *App) Config() *config.Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.Clone()
}

// DataDir returns the data directory in use. It only changes on restart.
func (a *App) DataDir() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.DataDir
}

// ApplyConfig validates and saves cfg, then restarts the station with it.
// Listen address and data directory changes take effect on the next process
// restart.
func (a *App) ApplyConfig(cfg *config.Config) error {
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := cfg.Save(a.ConfigPath); err != nil {
		return err
	}
	running := cfg.Clone()
	running.Listen, running.DataDir = a.cfg.Listen, a.cfg.DataDir
	a.Log.Info("configuration changed; restarting station")
	a.stopLocked()
	a.cfg = running
	a.startLocked()
	return nil
}
