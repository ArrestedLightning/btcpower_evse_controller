package device

import (
	"context"
	"math"
	"math/rand"
	"sync"
	"time"

	"github.com/ArrestedLightning/btcpower_evse_controller/internal/proto"
)

// Simulated board state codes (see proto.ClassifyState).
const (
	simIdle      = 2
	simConnected = 3
	simCharging  = 4
)

// SimCharger is a simulated charge board with a simulated vehicle. In auto
// mode, vehicles arrive, charge and leave on their own.
type SimCharger struct {
	mu          sync.Mutex
	auto        bool
	enable      bool
	duty        int
	plugged     bool
	requesting  bool
	fault       bool
	carMaxAmps  float64
	capacityWh  float64
	deliveredWh float64 // Wh delivered this session
	current     float64
	energyWh    float64 // lifetime, read by the meter
	nextEvent   time.Time
	updated     time.Time
	rng         *rand.Rand
}

// NewSimCharger creates a simulated board.
func NewSimCharger(auto bool, seed int64) *SimCharger {
	r := rand.New(rand.NewSource(seed))
	return &SimCharger{auto: auto, rng: r, nextEvent: time.Now().Add(time.Duration(3+r.Intn(20)) * time.Second)}
}

// SetAuto switches between automatic vehicles and manual control.
func (s *SimCharger) SetAuto(auto bool) {
	s.mu.Lock()
	s.auto = auto
	s.nextEvent = time.Now().Add(5 * time.Second)
	s.mu.Unlock()
}

func (s *SimCharger) SetControl(enable bool, duty int) {
	s.mu.Lock()
	s.enable, s.duty = enable, duty
	s.mu.Unlock()
}

// Plug connects a vehicle that will request charge.
func (s *SimCharger) Plug() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.plugLocked()
	s.requesting = true
}

func (s *SimCharger) plugLocked() {
	s.plugged, s.fault = true, false
	s.carMaxAmps = []float64{16, 24, 32, 40, 48}[s.rng.Intn(5)]
	s.capacityWh = 1000 + s.rng.Float64()*3000 // small so demo sessions finish
	s.deliveredWh = 0
}

// Unplug disconnects the vehicle.
func (s *SimCharger) Unplug() {
	s.mu.Lock()
	s.plugged, s.requesting, s.current = false, false, 0
	s.mu.Unlock()
}

// SetRequesting makes the plugged-in vehicle request (or stop requesting) charge.
func (s *SimCharger) SetRequesting(r bool) {
	s.mu.Lock()
	if s.plugged {
		s.requesting = r
	}
	s.mu.Unlock()
}

// SetFault toggles a simulated board fault.
func (s *SimCharger) SetFault(f bool) {
	s.mu.Lock()
	s.fault = f
	s.mu.Unlock()
}

func (s *SimCharger) Run(ctx context.Context) {
	const dt = 250 * time.Millisecond
	t := time.NewTicker(dt)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.step(now, dt)
		}
	}
}

func (s *SimCharger) step(now time.Time, dt time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updated = now
	if s.auto && now.After(s.nextEvent) {
		switch {
		case !s.plugged:
			s.plugLocked()
			s.nextEvent = now.Add(time.Duration(2+s.rng.Intn(4)) * time.Second)
		case !s.requesting && s.deliveredWh < s.capacityWh:
			s.requesting = true
			s.nextEvent = now.Add(time.Hour) // until full
		default:
			s.plugged, s.requesting = false, false
			s.nextEvent = now.Add(time.Duration(15+s.rng.Intn(60)) * time.Second)
		}
	}
	// Vehicle demand tapers over the last 20% of the session.
	demand := 0.0
	if s.plugged && s.requesting && s.enable && !s.fault {
		demand = s.carMaxAmps
		if frac := s.deliveredWh / s.capacityWh; frac > 0.8 {
			demand *= math.Max(0, (1-frac)/0.2)
		}
		demand = math.Min(demand, proto.AmpsForDuty(s.duty))
		if demand < 1 {
			// Full: stop requesting and leave a while later.
			s.requesting, demand = false, 0
			if s.auto {
				s.nextEvent = now.Add(time.Duration(10+s.rng.Intn(30)) * time.Second)
			}
		}
	}
	// First-order ramp toward demand, as vehicles do.
	s.current += (demand - s.current) * 0.3
	if s.current < 0.05 {
		s.current = 0
	}
	// Energy is accelerated 10x so demo sessions finish in minutes.
	wh := 240 * s.current * dt.Hours() * 10
	s.deliveredWh += wh
	s.energyWh += wh
}

func (s *SimCharger) Reading() ChargerReading {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := proto.Status{
		State:          simIdle,
		PilotState:     1,
		DutyCycle:      uint8(s.duty),
		Enabled:        s.enable,
		MinusVoltageOK: true,
		PilotVoltsPlus: 12,
		PilotVoltsMin:  -12,
	}
	switch {
	case s.fault:
		st.State, st.FaultCode = 5, 7
	case s.plugged && s.current > 0:
		st.State, st.PilotState, st.Contactor, st.ACOutPresent, st.PilotVoltsPlus = simCharging, 3, true, true, 6
	case s.plugged:
		st.State, st.PilotState, st.PilotVoltsPlus = simConnected, 2, 9
		if s.requesting && s.enable {
			st.State, st.PilotState, st.Contactor, st.PilotVoltsPlus = simCharging, 3, true, 6
		}
	}
	return ChargerReading{Status: st, Firmware: "SIM", Updated: s.updated}
}

func (s *SimCharger) measure() (amps, wh float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current, s.energyWh
}

// SimMeter is a simulated meter measuring up to two simulated chargers.
type SimMeter struct {
	Channels [2]*SimCharger // nil for unused channels

	mu      sync.Mutex
	counter uint8
	updated time.Time
}

func (m *SimMeter) Run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			m.mu.Lock()
			m.counter++
			m.updated = now
			m.mu.Unlock()
		}
	}
}

func (m *SimMeter) Reading() MeterReading {
	m.mu.Lock()
	r := MeterReading{Updated: m.updated, Frame: proto.MeterFrame{Firmware: "SIM", Counter: m.counter}}
	m.mu.Unlock()
	for i, c := range m.Channels {
		v := 240.0
		ch := proto.MeterChannel{Voltage: v, FrequencyRaw: 6000}
		if c != nil {
			amps, wh := c.measure()
			ch.Current = math.Round(amps*1000) / 1000
			ch.ActivePower = v * ch.Current
			ch.ApparentPower = ch.ActivePower
			total := uint64(wh * 100) // 0.01 Wh units
			ch.EnergyRaw, ch.EnergyOverflow = uint32(total), uint32(total>>32)
		}
		r.Frame.Channels[i] = ch
	}
	return r
}
