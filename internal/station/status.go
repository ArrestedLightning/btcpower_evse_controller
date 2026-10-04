package station

import (
	"time"

	"github.com/ArrestedLightning/btcpower_evse_controller/internal/proto"
)

// Status is the snapshot served by the API.
type Status struct {
	StationName string            `json:"station_name"`
	Time        time.Time         `json:"time"`
	Simulated   bool              `json:"simulated"`
	LoadSharing LoadSharingStatus `json:"load_sharing"`
	Ports       []PortStatus      `json:"ports"`
	Meters      []MeterStatus     `json:"meters"`
}

// LoadSharingStatus summarises the current budget.
type LoadSharingStatus struct {
	Enabled       bool     `json:"enabled"`
	Dynamic       bool     `json:"dynamic"`
	SystemMaxAmps float64  `json:"system_max_amps"`
	AllocatedAmps float64  `json:"allocated_amps"` // sum offered to vehicles
	MeasuredAmps  *float64 `json:"measured_amps"`  // sum measured, if all active ports are metered
}

// PortStatus describes one charging port.
type PortStatus struct {
	ID             int               `json:"id"`
	Name           string            `json:"name"`
	State          proto.ChargeState `json:"state"`
	StateCode      uint8             `json:"state_code"`
	FaultCode      uint8             `json:"fault_code"`
	Online         bool              `json:"online"`
	Enabled        bool              `json:"enabled"`         // user setting
	ChargingActive bool              `json:"charging_active"` // board is currently being told to enable
	Paused         bool              `json:"paused"`          // waiting for capacity
	Throttled      bool              `json:"throttled"`
	MaxAmps        float64           `json:"max_amps"`
	OfferedAmps    float64           `json:"offered_amps"` // advertised via the pilot
	FairShareAmps  float64           `json:"fair_share_amps"`
	CommandedDuty  int               `json:"commanded_duty"`
	Firmware       string            `json:"firmware,omitempty"`
	LastUpdate     *time.Time        `json:"last_update,omitempty"`
	Error          string            `json:"error,omitempty"`
	Meter          *PortMeter        `json:"meter,omitempty"`
	Session        *SessionStatus    `json:"session,omitempty"`
	Board          *proto.Status     `json:"board,omitempty"`
}

// PortMeter holds the meter readings for a port.
type PortMeter struct {
	Fresh          bool    `json:"fresh"`
	Voltage        float64 `json:"voltage"`
	Current        float64 `json:"current"`
	PowerW         float64 `json:"power_w"`
	EnergyTotalKWh float64 `json:"energy_total_kwh"`
}

// SessionStatus describes the in-progress session on a port.
type SessionStatus struct {
	PluggedIn       time.Time  `json:"plugged_in"`
	ChargeStart     *time.Time `json:"charge_start,omitempty"`
	DurationSeconds float64    `json:"duration_seconds"`
	ChargingSeconds float64    `json:"charging_seconds"`
	EnergyKWh       *float64   `json:"energy_kwh"`
}

// MeterStatus describes one meter board.
type MeterStatus struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Online   bool   `json:"online"`
	Firmware string `json:"firmware,omitempty"`
	Error    string `json:"error,omitempty"`
}

// buildStatus assembles a snapshot; s.mu must be held.
func (s *Station) buildStatus(now time.Time) Status {
	ls := s.cfg.LoadSharing
	st := Status{
		StationName: s.cfg.StationName,
		Time:        now,
		Simulated:   len(s.sims) > 0,
		LoadSharing: LoadSharingStatus{Enabled: ls.Enabled, Dynamic: ls.Enabled && ls.Dynamic, SystemMaxAmps: ls.SystemMaxAmps},
	}
	measured, allMetered := 0.0, true
	for i, c := range s.chargers {
		r := c.Reading()
		p := &s.ports[i]
		ps := PortStatus{
			ID:             i + 1,
			Name:           s.cfg.Chargers[i].Name,
			State:          proto.StateOffline,
			Online:         r.Fresh(now),
			Enabled:        s.enabled[i],
			ChargingActive: p.enable,
			Throttled:      p.out.Throttled,
			MaxAmps:        s.cfg.Chargers[i].MaxAmps,
			FairShareAmps:  p.out.FairShare,
			CommandedDuty:  p.duty,
			Firmware:       r.Firmware,
			Error:          r.Err,
		}
		if !r.Updated.IsZero() {
			t := r.Updated
			ps.LastUpdate = &t
			board := r.Status
			ps.Board = &board
		}
		if ps.Online {
			ps.State = proto.ClassifyState(r.Status.State)
			ps.StateCode = r.Status.State
			ps.FaultCode = r.Status.FaultCode
			ps.Error = ""
		}
		vehicle := ps.State == proto.StateConnected || ps.State == proto.StateCharging
		ps.Paused = vehicle && ps.Enabled && p.out.Paused
		if p.enable {
			ps.OfferedAmps = proto.AmpsForDuty(p.duty)
			if vehicle {
				st.LoadSharing.AllocatedAmps += ps.OfferedAmps
			}
		}
		if ch, ok, fresh := s.meterChannel(i, now); ok {
			ps.Meter = &PortMeter{Fresh: fresh, Voltage: ch.Voltage, Current: ch.Current, PowerW: ch.ActivePower, EnergyTotalKWh: ch.EnergyKWh()}
			if fresh {
				measured += ch.Current
			} else {
				allMetered = false
			}
		} else if vehicle {
			allMetered = false
		}
		if sess := p.session; sess != nil {
			ps.Session = &SessionStatus{
				PluggedIn:       sess.PluggedIn,
				ChargeStart:     sess.ChargeStart,
				DurationSeconds: now.Sub(sess.PluggedIn).Seconds(),
				ChargingSeconds: sess.ChargingSeconds,
				EnergyKWh:       sess.EnergyKWh,
			}
		}
		st.Ports = append(st.Ports, ps)
	}
	if allMetered && len(s.meters) > 0 {
		st.LoadSharing.MeasuredAmps = &measured
	}
	for i, m := range s.meters {
		r := m.Reading()
		st.Meters = append(st.Meters, MeterStatus{
			ID:       i + 1,
			Name:     s.cfg.Meters[i].Name,
			Online:   r.Fresh(now),
			Firmware: r.Frame.Firmware,
			Error:    r.Err,
		})
	}
	return st
}
