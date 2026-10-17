package station

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ArrestedLightning/btcpower_evse_controller/internal/config"
	"github.com/ArrestedLightning/btcpower_evse_controller/internal/device"
	"github.com/ArrestedLightning/btcpower_evse_controller/internal/proto"
)

type fakeCharger struct {
	reading device.ChargerReading
	enable  bool
	duty    int
}

func (f *fakeCharger) Run(ctx context.Context)          { <-ctx.Done() }
func (f *fakeCharger) Reading() device.ChargerReading   { return f.reading }
func (f *fakeCharger) SetControl(enable bool, duty int) { f.enable, f.duty = enable, duty }

type fakeMeter struct{ reading device.MeterReading }

func (f *fakeMeter) Run(ctx context.Context)      { <-ctx.Done() }
func (f *fakeMeter) Reading() device.MeterReading { return f.reading }

func setup(t *testing.T, sharing bool) (*Station, []*fakeCharger, *fakeMeter, *Store) {
	t.Helper()
	cfg := &config.Config{
		Chargers: []config.Charger{
			{Serial: "a", MaxAmps: 32, Meter: &config.MeterRef{Meter: 1, Channel: 1}},
			{Serial: "b", MaxAmps: 32, Meter: &config.MeterRef{Meter: 1, Channel: 2}},
		},
		Meters:      []config.Meter{{Serial: "m"}},
		LoadSharing: config.LoadSharing{Enabled: sharing, SystemMaxAmps: 40},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fc := []*fakeCharger{{}, {}}
	fm := &fakeMeter{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := newWithDevices(cfg, Options{Log: log, Store: store}, []device.Charger{fc[0], fc[1]}, []device.Meter{fm})
	return s, fc, fm, store
}

func reading(state uint8, at time.Time) device.ChargerReading {
	return device.ChargerReading{Status: proto.Status{State: state}, Updated: at}
}

func meter(at time.Time, amps1, kwh1 float64) device.MeterReading {
	var f proto.MeterFrame
	f.Channels[0] = proto.MeterChannel{Voltage: 240, Current: amps1, ActivePower: 240 * amps1, EnergyRaw: uint32(kwh1 * 100000)}
	return device.MeterReading{Frame: f, Updated: at}
}

func TestNeverSeenBoardStaysDisabled(t *testing.T) {
	s, fc, _, _ := setup(t, false)
	s.cycle(time.Now())
	if fc[0].enable || fc[1].enable {
		t.Fatal("offline board enabled")
	}
	if got := s.Snapshot().Ports[0].State; got != proto.StateOffline {
		t.Fatalf("state %s", got)
	}
}

func TestSessionLifecycleAndControl(t *testing.T) {
	s, fc, fm, store := setup(t, false)
	now := time.Now()
	fc[0].reading, fc[1].reading = reading(2, now), reading(2, now)
	fm.reading = meter(now, 0, 100)
	s.cycle(now)
	if !fc[0].enable || fc[0].duty != proto.DutyForAmps(32) {
		t.Fatalf("control: enable %v duty %d", fc[0].enable, fc[0].duty)
	}

	// Plug in and charge for 10 cycles, drawing 30 A; energy rises 1.5 kWh.
	for i := 1; i <= 10; i++ {
		now = now.Add(time.Second)
		fc[0].reading, fc[1].reading = reading(4, now), reading(2, now)
		fm.reading = meter(now, 30, 100+0.15*float64(i))
		s.cycle(now)
	}
	snap := s.Snapshot().Ports[0]
	if snap.State != proto.StateCharging || snap.Session == nil || snap.Session.EnergyKWh == nil {
		t.Fatalf("snapshot: %+v", snap)
	}
	if e := *snap.Session.EnergyKWh; e < 1.34 || e > 1.36 {
		t.Fatalf("session energy %v, want 1.35", e)
	}

	// Unplug: once the end delay has passed, the session is written to the
	// store, ending at the moment the vehicle left.
	now = now.Add(time.Second)
	unplugged := now
	fc[0].reading = reading(2, now)
	s.cycle(now)
	if len(store.Recent(10)) != 0 {
		t.Fatal("session ended before the end delay")
	}
	now = now.Add(s.cfg.SessionEndDelay())
	fc[0].reading = reading(2, now)
	s.cycle(now)
	sessions := store.Recent(10)
	if len(sessions) == 1 && !sessions[0].PluggedOut.Equal(unplugged) {
		t.Fatalf("plugged out %v, want %v", sessions[0].PluggedOut, unplugged)
	}
	if len(sessions) != 1 || sessions[0].Port != 1 || sessions[0].PeakAmps == nil || *sessions[0].PeakAmps != 30 {
		t.Fatalf("sessions: %+v", sessions)
	}
	if sessions[0].ChargingSeconds < 8 || sessions[0].ChargeEnd == nil {
		t.Fatalf("charging time: %+v", sessions[0])
	}

	// Sessions survive a reload of the store.
	again, err := OpenStore(store.dir)
	if err != nil || len(again.Recent(10)) != 1 {
		t.Fatalf("reload: %v %v", err, again.Recent(10))
	}
}

func TestUserDisableAndPersist(t *testing.T) {
	s, fc, _, store := setup(t, false)
	now := time.Now()
	fc[0].reading, fc[1].reading = reading(2, now), reading(2, now)
	if err := s.SetEnabled(2, false); err != nil {
		t.Fatal(err)
	}
	s.cycle(now)
	if !fc[0].enable || fc[1].enable {
		t.Fatalf("enable: %v %v", fc[0].enable, fc[1].enable)
	}
	if en := store.LoadEnabled(2); !en[0] || en[1] {
		t.Fatalf("persisted %v", en)
	}
	if err := s.SetEnabled(3, true); err == nil {
		t.Fatal("port 3 accepted")
	}
}

func TestLoadSharingAppliedToBoards(t *testing.T) {
	s, fc, fm, _ := setup(t, true)
	now := time.Now()
	fc[0].reading, fc[1].reading = reading(4, now), reading(4, now)
	fm.reading = meter(now, 20, 0)
	s.cycle(now)
	want := proto.DutyForAmps(20)
	if fc[0].duty != want || fc[1].duty != want || !fc[0].enable || !fc[1].enable {
		t.Fatalf("duties %d %d, want %d", fc[0].duty, fc[1].duty, want)
	}
	if a := s.Snapshot().LoadSharing.AllocatedAmps; a > 40 {
		t.Fatalf("allocated %v", a)
	}
}

func TestOfflineKeepsLastCommand(t *testing.T) {
	s, fc, _, _ := setup(t, false)
	now := time.Now()
	fc[0].reading, fc[1].reading = reading(4, now), reading(2, now)
	s.cycle(now)
	fc[0].SetControl(false, 0)         // detect any re-send
	s.cycle(now.Add(10 * time.Second)) // readings now stale
	if fc[0].enable || fc[0].duty != 0 {
		t.Fatal("control changed while offline")
	}
	if s.Snapshot().Ports[0].Session == nil {
		t.Fatal("session closed while offline")
	}
}

// chargeFor runs n one-second cycles with port 1 in the given state, with the
// meter energy rising 0.1 kWh per cycle while charging.
func chargeFor(s *Station, fc []*fakeCharger, fm *fakeMeter, now *time.Time, kwh *float64, state uint8, n int) {
	for i := 0; i < n; i++ {
		*now = now.Add(time.Second)
		amps := 0.0
		if state == 4 {
			*kwh += 0.1
			amps = 30
		}
		fc[0].reading, fc[1].reading = reading(state, *now), reading(2, *now)
		fm.reading = meter(*now, amps, *kwh)
		s.cycle(*now)
	}
}

func TestBriefDisconnectKeepsSession(t *testing.T) {
	s, fc, fm, store := setup(t, false)
	now, kwh := time.Now(), 50.0
	chargeFor(s, fc, fm, &now, &kwh, 4, 10)
	chargeFor(s, fc, fm, &now, &kwh, 2, 3) // board briefly reports unplugged
	if p := s.Snapshot().Ports[0]; p.Session == nil || p.LastSession != nil {
		t.Fatalf("session ended during brief disconnect: %+v", p)
	}
	chargeFor(s, fc, fm, &now, &kwh, 4, 5)
	p := s.Snapshot().Ports[0]
	if p.Session == nil || *p.Session.EnergyKWh < 1.39 || *p.Session.EnergyKWh > 1.41 {
		t.Fatalf("energy not carried over: %+v", p.Session)
	}
	if len(store.Recent(10)) != 0 {
		t.Fatal("brief disconnect logged a session")
	}
}

func TestLastSessionShownAfterEnd(t *testing.T) {
	s, fc, fm, store := setup(t, false)
	now, kwh := time.Now(), 0.0
	chargeFor(s, fc, fm, &now, &kwh, 4, 20)
	chargeFor(s, fc, fm, &now, &kwh, 2, 15)
	p := s.Snapshot().Ports[0]
	if p.Session != nil || p.LastSession == nil {
		t.Fatalf("session not finished: %+v", p)
	}
	ls := p.LastSession
	if ls.EnergyKWh == nil || *ls.EnergyKWh < 1.89 || *ls.EnergyKWh > 1.91 { // 20 cycles; start read after the first
		t.Fatalf("last session energy %v", ls.EnergyKWh)
	}
	// Ended when the vehicle left, 15 s before the last cycle.
	if ls.SecondsSinceEnd < 14 || ls.SecondsSinceEnd > 15 {
		t.Fatalf("seconds since end %v", ls.SecondsSinceEnd)
	}
	if s.Snapshot().Ports[1].LastSession != nil {
		t.Fatal("port 2 has a last session")
	}

	// A restarted station shows the same last session.
	s2 := newWithDevices(s.cfg, Options{Log: s.log, Store: store}, []device.Charger{fc[0], fc[1]}, []device.Meter{fm})
	s2.cycle(now)
	if ls2 := s2.Snapshot().Ports[0].LastSession; ls2 == nil || *ls2.EnergyKWh != *ls.EnergyKWh {
		t.Fatalf("last session after restart: %+v", ls2)
	}
}

func TestZeroEndDelayEndsImmediately(t *testing.T) {
	s, fc, fm, store := setup(t, false)
	zero := 0
	s.cfg.SessionEndDelaySec = &zero
	now, kwh := time.Now(), 0.0
	chargeFor(s, fc, fm, &now, &kwh, 4, 3)
	chargeFor(s, fc, fm, &now, &kwh, 2, 1)
	if len(store.Recent(10)) != 1 {
		t.Fatal("session not ended immediately")
	}
}

func TestSessionsFileRotation(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	store.maxBytes = 1000
	for i := 0; i < 30; i++ {
		if err := store.AppendSession(Session{Port: 1 + i%2, PortName: "Port", PluggedIn: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, sessionsFile)
	for _, p := range []string{path, path + ".1"} {
		fi, err := os.Stat(p)
		if err != nil || fi.Size() > 1000 {
			t.Fatalf("%s: %v %v", p, fi, err)
		}
	}
	// Reopening reads both files, oldest first.
	again, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := len(again.Recent(0))
	if n < 5 || n >= 30 {
		t.Fatalf("reloaded %d sessions", n)
	}
	if again.LastForPort(2) == nil {
		t.Fatal("no last session for port 2")
	}
}
