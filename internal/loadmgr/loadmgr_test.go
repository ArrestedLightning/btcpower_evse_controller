package loadmgr

import (
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/ArrestedLightning/btcpower_evse_controller/internal/config"
	"github.com/ArrestedLightning/btcpower_evse_controller/internal/proto"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func lsCfg(systemMax float64, dynamic bool) config.LoadSharing {
	c := config.Config{LoadSharing: config.LoadSharing{Enabled: true, SystemMaxAmps: systemMax, Dynamic: dynamic}}
	c.ApplyDefaults()
	return c.LoadSharing
}

func port(max float64, st proto.ChargeState, sessionMin int) PortInput {
	p := PortInput{MaxAmps: max, State: st, Enabled: true}
	if st != proto.StateIdle {
		p.SessionStart = t0.Add(time.Duration(sessionMin) * time.Minute)
	}
	return p
}

func targets(out []PortOutput) []float64 {
	r := make([]float64, len(out))
	for i, o := range out {
		r[i] = o.TargetAmps
		if o.Paused {
			r[i] = 0
		}
	}
	return r
}

func eq(t *testing.T, label string, got, want []float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %v, want %v", label, got, want)
	}
	for i := range got {
		if math.Abs(got[i]-want[i]) > 1e-6 {
			t.Fatalf("%s: got %v, want %v", label, got, want)
		}
	}
}

func TestDisabledGivesPortMax(t *testing.T) {
	m := New(config.LoadSharing{}, []float64{32, 40})
	out := m.Step(t0, []PortInput{port(32, proto.StateCharging, 0), port(40, proto.StateCharging, 1)})
	eq(t, "disabled", targets(out), []float64{32, 40})
}

func TestBasicSplitAndWaterFill(t *testing.T) {
	m := New(lsCfg(40, false), []float64{32, 32})
	out := m.Step(t0, []PortInput{port(32, proto.StateCharging, 0), port(32, proto.StateCharging, 1)})
	eq(t, "two charging", targets(out), []float64{20, 20})

	m = New(lsCfg(60, false), []float64{16, 32, 32})
	out = m.Step(t0, []PortInput{port(16, proto.StateCharging, 0), port(32, proto.StateCharging, 1), port(32, proto.StateCharging, 2)})
	eq(t, "water fill", targets(out), []float64{16, 22, 22})

	m = New(lsCfg(40, false), []float64{32, 32})
	out = m.Step(t0, []PortInput{port(32, proto.StateCharging, 0), port(32, proto.StateIdle, 0)})
	eq(t, "one charging, idle port disarmed", targets(out), []float64{32, 0})

	m = New(lsCfg(60, false), []float64{32, 32})
	out = m.Step(t0, []PortInput{port(32, proto.StateCharging, 0), port(32, proto.StateIdle, 0)})
	eq(t, "one charging, idle port armed from spare", targets(out), []float64{32, 12})
}

func TestPlugInIntoDisarmedPort(t *testing.T) {
	m := New(lsCfg(40, false), []float64{32, 32})
	in := []PortInput{port(32, proto.StateCharging, 0), port(32, proto.StateIdle, 0)}
	m.Step(t0, in)
	m.Step(t0.Add(10*time.Second), in)

	in[1] = port(32, proto.StateConnected, 1)
	now := t0.Add(20 * time.Second)
	out := m.Step(now, in)
	eq(t, "others reduce first", targets(out), []float64{28, 0})
	if !out[1].Pending {
		t.Fatal("admission not pending")
	}
	out = m.Step(now.Add(IncreaseHoldoff), in)
	eq(t, "then new port enabled", targets(out), []float64{28, 12})
}

func TestOverloadPausesNewestSession(t *testing.T) {
	m := New(lsCfg(30, false), []float64{32, 32, 32})
	out := m.Step(t0, []PortInput{
		port(32, proto.StateCharging, 5), // newest
		port(32, proto.StateCharging, 1),
		port(32, proto.StateCharging, 2),
	})
	if !out[0].Paused || out[1].Paused || out[2].Paused {
		t.Fatalf("pause decision: %+v", out)
	}
	eq(t, "overload", targets(out), []float64{0, 15, 15})
}

func TestConnectedVehicleReservesMinimum(t *testing.T) {
	m := New(lsCfg(40, false), []float64{32, 32})
	out := m.Step(t0, []PortInput{port(32, proto.StateCharging, 0), port(32, proto.StateConnected, 1)})
	eq(t, "connected reserve", targets(out), []float64{28, 12})
}

func TestIncreaseWaitsForHoldoff(t *testing.T) {
	m := New(lsCfg(40, false), []float64{32, 32})
	both := []PortInput{port(32, proto.StateCharging, 0), port(32, proto.StateCharging, 1)}
	m.Step(t0, both)

	one := []PortInput{port(32, proto.StateCharging, 0), port(32, proto.StateIdle, 0)}
	now := t0.Add(10 * time.Second)
	out := m.Step(now, one) // port 2 drops 20 -> 12: a decrease, so port 1 must wait
	if !out[0].Pending {
		t.Fatal("increase not held back")
	}
	eq(t, "held", targets(out), []float64{20, 0})
	out = m.Step(now.Add(5*time.Second), one)
	eq(t, "still held", targets(out), []float64{20, 0})
	out = m.Step(now.Add(IncreaseHoldoff), one)
	eq(t, "released", targets(out), []float64{32, 0})
}

func TestOfflinePortKeepsReservation(t *testing.T) {
	m := New(lsCfg(48, false), []float64{32, 32})
	m.Step(t0, []PortInput{port(32, proto.StateCharging, 0), port(32, proto.StateCharging, 1)})
	out := m.Step(t0.Add(10*time.Second), []PortInput{port(32, proto.StateCharging, 0), {MaxAmps: 32, State: proto.StateOffline, Enabled: true}})
	eq(t, "offline", targets(out), []float64{24, 24})
}

func TestUserDisabledPortExcluded(t *testing.T) {
	m := New(lsCfg(40, false), []float64{32, 32})
	p2 := port(32, proto.StateCharging, 1)
	p2.Enabled = false
	out := m.Step(t0, []PortInput{port(32, proto.StateCharging, 0), p2})
	eq(t, "user disabled", targets(out), []float64{32, 12})
}

func TestDynamicThrottleAndRestore(t *testing.T) {
	cfg := lsCfg(40, true)
	m := New(cfg, []float64{32, 32})
	in := []PortInput{port(32, proto.StateCharging, 0), port(32, proto.StateCharging, 1)}
	in[0].MeterFresh, in[1].MeterFresh = true, true
	in[0].MeasuredAmps, in[1].MeasuredAmps = 5, 20

	now := t0
	var out []PortOutput
	for s := 0; s <= cfg.ThrottleDelaySec; s++ {
		now = t0.Add(time.Duration(s) * time.Second)
		out = m.Step(now, in)
	}
	if !out[0].Throttled {
		t.Fatalf("port 1 not throttled: %+v", out)
	}
	eq(t, "throttle applied", targets(out), []float64{12, 20})
	out = m.Step(now.Add(IncreaseHoldoff), in)
	eq(t, "freed current reallocated", targets(out), []float64{12, 28})
	if out[0].FairShare != 20 {
		t.Fatalf("fair share %v", out[0].FairShare)
	}

	// Port 1 starts drawing up to its cap: restore its fair share. The
	// reduction on port 2 goes first.
	now = now.Add(IncreaseHoldoff)
	in[0].MeasuredAmps = 11.5
	out = m.Step(now.Add(time.Second), in)
	eq(t, "restore: decrease first", targets(out), []float64{12, 20})
	out = m.Step(now.Add(time.Second+IncreaseHoldoff), in)
	eq(t, "restore: then increase", targets(out), []float64{20, 20})
}

func TestDynamicProbe(t *testing.T) {
	cfg := lsCfg(40, true)
	m := New(cfg, []float64{32, 32})
	in := []PortInput{port(32, proto.StateCharging, 0), port(32, proto.StateCharging, 1)}
	in[0].MeterFresh, in[1].MeterFresh = true, true
	in[0].MeasuredAmps, in[1].MeasuredAmps = 2, 20
	var out []PortOutput
	for s := 0; s < cfg.ThrottleDelaySec+cfg.ProbeIntervalSec+10; s++ {
		out = m.Step(t0.Add(time.Duration(s)*time.Second), in)
		if s == cfg.ThrottleDelaySec+cfg.ProbeIntervalSec+1 && out[0].Throttled {
			t.Fatal("not probed after interval")
		}
	}
	_ = out
}

func TestDynamicIgnoresStaleMeter(t *testing.T) {
	cfg := lsCfg(40, true)
	m := New(cfg, []float64{32, 32})
	in := []PortInput{port(32, proto.StateCharging, 0), port(32, proto.StateCharging, 1)}
	in[0].MeasuredAmps = 1 // MeterFresh false
	var out []PortOutput
	for s := 0; s < 60; s++ {
		out = m.Step(t0.Add(time.Duration(s)*time.Second), in)
	}
	if out[0].Throttled {
		t.Fatal("throttled on stale meter data")
	}
}

// TestRandomizedBudgetInvariant drives random state changes and checks that
// the current charging vehicles may draw never exceeds the budget. A vehicle
// may keep drawing an old, higher limit for up to 5 s after a reduction, so
// each port's draw is taken as the maximum it was offered over that window.
// The manager only sees a state change on the step after it happens, which
// models the one-cycle detection delay.
func TestRandomizedBudgetInvariant(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for trial := 0; trial < 50; trial++ {
		n := 1 + rng.Intn(4)
		systemMax := float64(24 + rng.Intn(80))
		maxes := make([]float64, n)
		for i := range maxes {
			maxes[i] = float64(12 + rng.Intn(64))
		}
		m := New(lsCfg(systemMax, rng.Intn(2) == 0), maxes)
		in := make([]PortInput, n)
		for i := range in {
			in[i] = PortInput{MaxAmps: maxes[i], State: proto.StateIdle, Enabled: true, MeterFresh: true}
		}
		type sample struct {
			at time.Time
			v  []float64
		}
		var hist []sample
		seen := append([]PortInput(nil), in...)
		for s := 0; s < 3000; s++ {
			now := t0.Add(time.Duration(s) * time.Second)
			for i := range in {
				if rng.Intn(20) != 0 {
					continue
				}
				switch in[i].State {
				case proto.StateIdle:
					// Some vehicles request charge immediately on plug-in.
					in[i].State = []proto.ChargeState{proto.StateConnected, proto.StateCharging}[rng.Intn(2)]
					in[i].SessionStart = now
				case proto.StateConnected:
					in[i].State = []proto.ChargeState{proto.StateCharging, proto.StateIdle}[rng.Intn(2)]
				case proto.StateCharging:
					in[i].State = []proto.ChargeState{proto.StateConnected, proto.StateIdle, proto.StateCharging}[rng.Intn(3)]
				}
				if in[i].State == proto.StateIdle {
					in[i].SessionStart = time.Time{}
				}
				in[i].MeasuredAmps = rng.Float64() * maxes[i]
			}
			out := m.Step(now, seen) // manager acts on last cycle's observation
			copy(seen, in)
			v := make([]float64, n)
			for i, o := range out {
				if !o.Paused {
					v[i] = o.TargetAmps
				}
			}
			hist = append(hist, sample{now, v})
			// A charging vehicle may draw the max offered within the last 5 s.
			total := 0.0
			for i := range in {
				if in[i].State != proto.StateCharging {
					continue
				}
				peak := 0.0
				for k := len(hist) - 1; k >= 0 && now.Sub(hist[k].at) <= 5*time.Second; k-- {
					peak = math.Max(peak, hist[k].v[i])
				}
				total += peak
			}
			if total > systemMax+1e-6 {
				t.Fatalf("trial %d step %d: draw %.1f exceeds budget %.1f (maxes %v)", trial, s, total, systemMax, maxes)
			}
		}
	}
}
