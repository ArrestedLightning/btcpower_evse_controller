// Package loadmgr decides the current limit advertised on each port.
//
// It is pure logic with no I/O: the caller feeds it a snapshot of every port
// once per control cycle and applies the returned targets.
//
// Safety rules:
//   - The sum of currents that vehicles may draw never exceeds the system
//     budget: reductions are applied immediately, while increases (including
//     un-pausing a port) wait until no reduction has happened for
//     IncreaseHoldoff, giving vehicles time to respond (J1772 allows 5 s).
//   - Offline or unrecognised boards are assumed to keep drawing their last
//     advertised current.
//   - Vehicles connected but not requesting charge (J1772 state B) are
//     reserved MinAmps so that they can start without exceeding the budget.
//   - Idle ports stay enabled only when spare capacity covers MinAmps;
//     otherwise they are disabled until a plugged-in vehicle is admitted.
//
// Ports that cannot be given MinAmps are paused (charging disabled), newest
// session first.
package loadmgr

import (
	"math"
	"sort"
	"time"

	"github.com/ArrestedLightning/btcpower_evse_controller/internal/config"
	"github.com/ArrestedLightning/btcpower_evse_controller/internal/proto"
)

// IncreaseHoldoff is how long after any reduction before increases are sent.
const IncreaseHoldoff = 6 * time.Second

// restoreMarginAmps: a throttled port drawing within this much of its cap is
// restored to its fair share.
const restoreMarginAmps = 1.0

// PortInput is the state of one port for a control cycle.
type PortInput struct {
	MaxAmps      float64
	State        proto.ChargeState
	Enabled      bool      // user enable flag
	SessionStart time.Time // when the vehicle was plugged in; zero if idle
	MeasuredAmps float64
	MeterFresh   bool // MeasuredAmps is valid and recent
}

// PortOutput is the decision for one port.
type PortOutput struct {
	// TargetAmps is the current to advertise. When Paused it is the value
	// the board is left at while charging is disabled.
	TargetAmps float64 `json:"target_amps"`
	// Paused means charging must be disabled for lack of capacity.
	Paused bool `json:"paused"`
	// Throttled means dynamic sharing lowered the limit below the fair share.
	Throttled bool `json:"throttled"`
	// FairShare is the basic-mode allocation for this port.
	FairShare float64 `json:"fair_share"`
	// Pending is true when an increase is being held back.
	Pending bool `json:"pending"`
}

type portState struct {
	advertised    float64 // what the vehicle may currently draw (0 when paused)
	paused        bool
	throttled     bool
	throttleCap   float64
	underuseSince time.Time
	throttledAt   time.Time
	session       time.Time
}

// Manager holds state between control cycles.
type Manager struct {
	cfg          config.LoadSharing
	ports        []portState
	lastDecrease time.Time
}

// New creates a manager for n ports. Until told otherwise, every port is
// assumed to be advertising its maximum current.
func New(cfg config.LoadSharing, maxAmps []float64) *Manager {
	m := &Manager{cfg: cfg, ports: make([]portState, len(maxAmps))}
	for i, a := range maxAmps {
		m.ports[i].advertised = a
	}
	return m
}

// Step computes the targets for one control cycle.
func (m *Manager) Step(now time.Time, in []PortInput) []PortOutput {
	out := make([]PortOutput, len(in))
	if !m.cfg.Enabled {
		for i, p := range in {
			out[i] = PortOutput{TargetAmps: p.MaxAmps, FairShare: p.MaxAmps}
			m.ports[i].advertised = p.MaxAmps
		}
		return out
	}

	minA := m.cfg.MinAmps
	budget := m.cfg.SystemMaxAmps
	desired := make([]float64, len(in))
	paused := make([]bool, len(in))

	// Classify ports. Ports not taking part are pre-set to MinAmps so a newly
	// plugged vehicle starts low.
	var charging, connected, idle []int
	for i, p := range in {
		ps := &m.ports[i]
		active := p.Enabled && (p.State == proto.StateCharging || p.State == proto.StateConnected)
		if !p.SessionStart.Equal(ps.session) {
			ps.session = p.SessionStart
			m.clearThrottle(ps)
		}
		switch {
		case p.State == proto.StateOffline || p.State == proto.StateUnknown:
			// Can't control it; assume it keeps its last advertised current.
			budget -= ps.advertised
			desired[i] = ps.advertised
			paused[i] = ps.paused
		case !active:
			desired[i] = math.Min(minA, p.MaxAmps)
			m.clearThrottle(ps)
			if p.Enabled {
				idle = append(idle, i)
			}
		case p.State == proto.StateCharging:
			charging = append(charging, i)
		default:
			connected = append(connected, i)
			m.clearThrottle(ps)
		}
	}

	// Admission: oldest charging sessions first, then connected vehicles.
	byAge := func(ix []int) {
		sort.SliceStable(ix, func(a, b int) bool {
			sa, sb := in[ix[a]].SessionStart, in[ix[b]].SessionStart
			if !sa.Equal(sb) {
				return sa.Before(sb)
			}
			return ix[a] < ix[b]
		})
	}
	byAge(charging)
	byAge(connected)
	if budget < 0 {
		budget = 0
	}
	slots := int(math.Floor(budget/minA + 1e-9))
	var admitted []int
	for _, i := range charging {
		if slots > 0 {
			admitted = append(admitted, i)
			slots--
		} else {
			paused[i], desired[i] = true, 0
		}
	}
	// Charging ports share whatever is not reserved for connected vehicles.
	shareBudget := budget
	for _, i := range connected {
		if slots > 0 {
			desired[i] = math.Min(minA, in[i].MaxAmps)
			shareBudget -= desired[i]
			slots--
		} else {
			paused[i], desired[i] = true, 0
		}
	}

	// Basic fair share (ignoring throttling), used for display and as the
	// level throttled ports are restored to.
	fair := waterFill(shareBudget, capsOf(in, admitted))
	for k, i := range admitted {
		out[i].FairShare = fair[k]
	}

	// Dynamic: throttle under-using ports and redistribute.
	if m.cfg.Dynamic {
		caps := make([]float64, len(admitted))
		for k, i := range admitted {
			caps[k] = m.updateThrottle(now, &m.ports[i], in[i], fair[k])
		}
		alloc := waterFill(shareBudget, caps)
		for k, i := range admitted {
			desired[i] = alloc[k]
			out[i].Throttled = m.ports[i].throttled
		}
	} else {
		for k, i := range admitted {
			desired[i] = fair[k]
		}
	}

	// Idle ports stay enabled ("armed") only if spare capacity covers their
	// pre-set minimum, since a vehicle could plug in and draw it before the
	// next cycle. Otherwise they are held disabled, and a vehicle that plugs
	// in is admitted like any paused port: after other ports have reduced.
	spare := budget
	for i := range in {
		if (in[i].State == proto.StateCharging || in[i].State == proto.StateConnected) && !paused[i] && in[i].Enabled {
			spare -= desired[i]
		}
	}
	for _, i := range idle {
		if spare >= desired[i]-1e-9 {
			spare -= desired[i]
		} else {
			paused[i] = true
		}
	}

	// Apply decreases now, increases only after the holdoff.
	for i := range in {
		ps := &m.ports[i]
		effective := desired[i]
		if paused[i] {
			effective = 0
		}
		if effective < ps.advertised-1e-9 {
			m.lastDecrease = now
		}
	}
	for i := range in {
		ps := &m.ports[i]
		effective := desired[i]
		if paused[i] {
			effective = 0
		}
		switch {
		case effective <= ps.advertised+1e-9:
			ps.advertised, ps.paused = effective, paused[i]
		case now.Sub(m.lastDecrease) >= IncreaseHoldoff:
			ps.advertised, ps.paused = effective, paused[i]
		default:
			out[i].Pending = true
		}
		out[i].Paused = ps.paused
		out[i].TargetAmps = ps.advertised
		if ps.paused {
			// Board is disabled; leave it pre-set to the minimum.
			out[i].TargetAmps = math.Min(minA, in[i].MaxAmps)
		}
	}
	return out
}

// updateThrottle advances the dynamic throttle state for a charging port and
// returns its cap.
func (m *Manager) updateThrottle(now time.Time, ps *portState, p PortInput, fair float64) float64 {
	if !p.MeterFresh {
		m.clearThrottle(ps)
		return p.MaxAmps
	}
	minA := m.cfg.MinAmps
	current := fair
	if ps.throttled {
		current = ps.throttleCap
		probe := time.Duration(m.cfg.ProbeIntervalSec) * time.Second
		if p.MeasuredAmps >= ps.throttleCap-restoreMarginAmps || now.Sub(ps.throttledAt) >= probe {
			m.clearThrottle(ps)
			ps.underuseSince = now // must under-use for a full delay again
			return p.MaxAmps
		}
	}
	proposed := math.Max(minA, math.Ceil(p.MeasuredAmps+m.cfg.ThrottleHeadroomAmps))
	if p.MeasuredAmps < current-m.cfg.ThrottleMarginAmps && proposed < current {
		if ps.underuseSince.IsZero() {
			ps.underuseSince = now
		}
		if now.Sub(ps.underuseSince) >= time.Duration(m.cfg.ThrottleDelaySec)*time.Second {
			if !ps.throttled {
				ps.throttledAt = now
			}
			ps.throttled = true
			ps.throttleCap = proposed
			ps.underuseSince = time.Time{}
		}
	} else {
		ps.underuseSince = time.Time{}
	}
	if ps.throttled {
		return math.Min(ps.throttleCap, p.MaxAmps)
	}
	return p.MaxAmps
}

func (m *Manager) clearThrottle(ps *portState) {
	ps.throttled = false
	ps.throttleCap = 0
	ps.underuseSince = time.Time{}
	ps.throttledAt = time.Time{}
}

func capsOf(in []PortInput, ix []int) []float64 {
	caps := make([]float64, len(ix))
	for k, i := range ix {
		caps[k] = in[i].MaxAmps
	}
	return caps
}

// waterFill divides budget among ports with the given caps: each gets an
// equal share, and any share above a port's cap is redistributed to the
// others. Results are rounded down to 0.1 A.
func waterFill(budget float64, caps []float64) []float64 {
	out := make([]float64, len(caps))
	order := make([]int, len(caps))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return caps[order[a]] < caps[order[b]] })
	remaining := budget
	for n, i := range order {
		share := remaining / float64(len(order)-n)
		v := math.Floor(math.Min(caps[i], share)*10+1e-9) / 10
		out[i] = v
		remaining -= v
	}
	return out
}
