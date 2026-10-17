// Package station runs the control loop: it reads the boards and meters,
// tracks sessions, applies load sharing and publishes status snapshots.
package station

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/ArrestedLightning/btcpower_evse_controller/internal/config"
	"github.com/ArrestedLightning/btcpower_evse_controller/internal/device"
	"github.com/ArrestedLightning/btcpower_evse_controller/internal/loadmgr"
	"github.com/ArrestedLightning/btcpower_evse_controller/internal/proto"
)

// Options configures a Station.
type Options struct {
	Simulate    bool
	DebugSerial bool
	Log         *slog.Logger
	Store       *Store
}

// Station owns the devices for one configuration.
type Station struct {
	cfg      *config.Config
	log      *slog.Logger
	store    *Store
	chargers []device.Charger
	meters   []device.Meter
	sims     []*device.SimCharger
	lm       *loadmgr.Manager
	interval time.Duration

	mu       sync.RWMutex
	enabled  []bool
	ports    []portRuntime
	snapshot Status
}

type portRuntime struct {
	session     *Session
	absentSince time.Time // when the vehicle was last seen leaving; zero if present
	last        *Session  // most recent finished session
	startEnergy float64
	haveStart   bool
	lastSeen    time.Time // last time the board was online
	lastClass   proto.ChargeState
	enable      bool
	duty        int
	out         loadmgr.PortOutput
}

// New creates the devices described by cfg. Nothing runs until Run.
func New(cfg *config.Config, opts Options) *Station {
	interval := time.Duration(cfg.PollIntervalMS) * time.Millisecond
	var chargers []device.Charger
	var meters []device.Meter
	var sims []*device.SimCharger
	for i, ch := range cfg.Chargers {
		if opts.Simulate {
			sim := device.NewSimCharger(true, int64(i+1)*7919)
			sims = append(sims, sim)
			chargers = append(chargers, sim)
		} else {
			chargers = append(chargers, device.NewSerialCharger(ch.Serial, interval, opts.DebugSerial, opts.Log))
		}
	}
	for mi, m := range cfg.Meters {
		if opts.Simulate {
			sm := &device.SimMeter{}
			for ci, ch := range cfg.Chargers {
				if ch.Meter != nil && ch.Meter.Meter == mi+1 {
					sm.Channels[ch.Meter.Channel-1] = sims[ci]
				}
			}
			meters = append(meters, sm)
		} else {
			meters = append(meters, device.NewSerialMeter(m.Serial, opts.DebugSerial, opts.Log))
		}
	}
	s := newWithDevices(cfg, opts, chargers, meters)
	s.sims = sims
	return s
}

func newWithDevices(cfg *config.Config, opts Options, chargers []device.Charger, meters []device.Meter) *Station {
	s := &Station{
		cfg:      cfg,
		log:      opts.Log,
		store:    opts.Store,
		chargers: chargers,
		meters:   meters,
		interval: time.Duration(cfg.PollIntervalMS) * time.Millisecond,
		ports:    make([]portRuntime, len(cfg.Chargers)),
		enabled:  opts.Store.LoadEnabled(len(cfg.Chargers)),
	}
	maxAmps := make([]float64, len(cfg.Chargers))
	for i, ch := range cfg.Chargers {
		maxAmps[i] = ch.MaxAmps
		s.ports[i].last = s.store.LastForPort(i + 1)
	}
	s.lm = loadmgr.New(cfg.LoadSharing, maxAmps)
	// Until a board has been seen, keep it disabled at minimum current.
	for i, c := range s.chargers {
		s.ports[i].duty = proto.MinDuty
		c.SetControl(false, proto.MinDuty)
	}
	s.snapshot = s.buildStatus(time.Now())
	return s
}

// Config returns the configuration this station was built from.
func (s *Station) Config() *config.Config { return s.cfg }

// Run drives all devices and the control loop until ctx is cancelled, and
// returns once every device has released its serial port.
func (s *Station) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, c := range s.chargers {
		wg.Add(1)
		go func(c device.Charger) { defer wg.Done(); c.Run(ctx) }(c)
	}
	for _, m := range s.meters {
		wg.Add(1)
		go func(m device.Meter) { defer wg.Done(); m.Run(ctx) }(m)
	}
	t := time.NewTicker(s.interval)
	defer t.Stop()
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case now := <-t.C:
			s.cycle(now)
		}
	}
	s.finishOpenSessions(time.Now())
	wg.Wait()
}

// meterChannel returns the reading for a port's meter channel, if any.
func (s *Station) meterChannel(i int, now time.Time) (proto.MeterChannel, bool, bool) {
	ref := s.cfg.Chargers[i].Meter
	if ref == nil {
		return proto.MeterChannel{}, false, false
	}
	r := s.meters[ref.Meter-1].Reading()
	return r.Frame.Channels[ref.Channel-1], true, r.Fresh(now)
}

func (s *Station) cycle(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	inputs := make([]loadmgr.PortInput, len(s.chargers))
	for i, c := range s.chargers {
		r := c.Reading()
		class := proto.StateOffline
		if r.Fresh(now) {
			class = proto.ClassifyState(r.Status.State)
			s.ports[i].lastSeen = now
		}
		ch, hasMeter, meterFresh := s.meterChannel(i, now)
		s.trackSession(i, now, class, ch, hasMeter && meterFresh)
		in := loadmgr.PortInput{
			MaxAmps:      s.cfg.Chargers[i].MaxAmps,
			State:        class,
			Enabled:      s.enabled[i],
			MeasuredAmps: ch.Current,
			MeterFresh:   hasMeter && meterFresh,
		}
		if sess := s.ports[i].session; sess != nil {
			in.SessionStart = sess.PluggedIn
		}
		inputs[i] = in
	}

	outs := s.lm.Step(now, inputs)
	for i, c := range s.chargers {
		p := &s.ports[i]
		p.out = outs[i]
		if inputs[i].State == proto.StateOffline {
			// Keep the last command; if never seen, it stays disabled.
			continue
		}
		p.enable = s.enabled[i] && !outs[i].Paused
		p.duty = clampDuty(proto.DutyForAmps(outs[i].TargetAmps))
		c.SetControl(p.enable, p.duty)
	}
	s.snapshot = s.buildStatus(now)
}

func clampDuty(d int) int {
	return max(proto.MinDuty, min(proto.MaxDuty, d))
}

// trackSession starts, updates and finishes the session on port i.
func (s *Station) trackSession(i int, now time.Time, class proto.ChargeState, ch proto.MeterChannel, meterOK bool) {
	p := &s.ports[i]
	prev := p.lastClass
	if class == proto.StateOffline || class == proto.StateUnknown {
		return // no information; leave any session open
	}
	p.lastClass = class
	vehicle := class == proto.StateConnected || class == proto.StateCharging
	if vehicle {
		if !p.absentSince.IsZero() && p.session != nil {
			s.log.Info("vehicle reconnected; continuing session", "port", i+1, "gap", now.Sub(p.absentSince).Round(time.Second))
		}
		p.absentSince = time.Time{}
	}
	if p.session == nil && vehicle {
		p.session = &Session{Port: i + 1, PortName: s.cfg.Chargers[i].Name, PluggedIn: now}
		p.haveStart = false
		s.log.Info("session started", "port", i+1)
	}
	sess := p.session
	if sess == nil {
		return
	}
	if meterOK {
		e := ch.EnergyKWh()
		if !p.haveStart {
			p.startEnergy, p.haveStart = e, true
		}
		used := math.Max(0, e-p.startEnergy)
		sess.EnergyKWh = &used
		peak := ch.Current
		if sess.PeakAmps != nil {
			peak = math.Max(peak, *sess.PeakAmps)
		}
		sess.PeakAmps = &peak
	}
	if class == proto.StateCharging {
		if sess.ChargeStart == nil {
			t := now
			sess.ChargeStart = &t
		}
		if prev == proto.StateCharging {
			sess.ChargingSeconds += s.interval.Seconds()
		}
		sess.ChargeEnd = nil
	} else if prev == proto.StateCharging && sess.ChargeEnd == nil {
		t := now
		sess.ChargeEnd = &t
	}
	if !vehicle {
		// End the session only once the vehicle has stayed away, since
		// boards briefly report "disconnected" when charging finishes.
		if p.absentSince.IsZero() {
			p.absentSince = now
		}
		if now.Sub(p.absentSince) >= s.cfg.SessionEndDelay() {
			s.finishSession(i, p.absentSince)
		}
	}
}

// finishSession records the session on port i as ended at end.
func (s *Station) finishSession(i int, end time.Time) {
	p := &s.ports[i]
	sess := p.session
	if sess == nil {
		return
	}
	sess.PluggedOut = &end
	if sess.ChargeStart != nil && sess.ChargeEnd == nil {
		sess.ChargeEnd = &end
	}
	p.session, p.absentSince, p.last = nil, time.Time{}, sess
	if err := s.store.AppendSession(*sess); err != nil {
		s.log.Error("saving session", "err", err)
	}
	s.log.Info("session finished", "port", i+1, "charging_seconds", sess.ChargingSeconds)
}

// finishOpenSessions records sessions still open at shutdown, so a restart
// (e.g. after a settings change) does not lose them.
func (s *Station) finishOpenSessions(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.ports {
		end := now
		if t := s.ports[i].absentSince; !t.IsZero() {
			end = t
		}
		s.finishSession(i, end)
	}
}

// Snapshot returns the latest status.
func (s *Station) Snapshot() Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshot
}

// SetEnabled changes and persists the user enable flag for port id (1-based).
func (s *Station) SetEnabled(id int, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id < 1 || id > len(s.enabled) {
		return fmt.Errorf("no port %d", id)
	}
	s.enabled[id-1] = enabled
	s.snapshot = s.buildStatus(time.Now())
	return s.store.SaveEnabled(s.enabled)
}

// Sessions returns recent finished sessions, newest first.
func (s *Station) Sessions(limit int) []Session { return s.store.Recent(limit) }

// Sim returns the simulated charger for port id (1-based), or nil.
func (s *Station) Sim(id int) *device.SimCharger {
	if id < 1 || id > len(s.sims) {
		return nil
	}
	return s.sims[id-1]
}
