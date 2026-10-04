package station

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ArrestedLightning/btcpower_evse_controller/internal/config"
)

const (
	stateFile    = "state.json"
	sessionsFile = "sessions.jsonl"
	// maxRecentSessions is how many finished sessions are kept in memory.
	maxRecentSessions = 200
)

// Session is a finished (or in-progress) charging session.
type Session struct {
	Port            int        `json:"port"`
	PortName        string     `json:"port_name"`
	PluggedIn       time.Time  `json:"plugged_in"`
	PluggedOut      *time.Time `json:"plugged_out,omitempty"`
	ChargeStart     *time.Time `json:"charge_start,omitempty"`
	ChargeEnd       *time.Time `json:"charge_end,omitempty"`
	ChargingSeconds float64    `json:"charging_seconds"`
	EnergyKWh       *float64   `json:"energy_kwh"` // nil without a meter
	PeakAmps        *float64   `json:"peak_amps"`  // nil without a meter
}

type persistedState struct {
	Ports []struct {
		Enabled bool `json:"enabled"`
	} `json:"ports"`
}

// Store persists per-port enable flags and the session log.
type Store struct {
	dir string

	mu     sync.Mutex
	recent []Session
}

// OpenStore creates dir if needed and loads recent sessions.
func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{dir: dir}
	f, err := os.Open(filepath.Join(dir, sessionsFile))
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var sess Session
		if json.Unmarshal(sc.Bytes(), &sess) == nil {
			s.recent = append(s.recent, sess)
			if len(s.recent) > maxRecentSessions {
				s.recent = s.recent[1:]
			}
		}
	}
	return s, sc.Err()
}

// LoadEnabled returns the persisted enable flags for n ports; ports without a
// saved flag default to enabled.
func (s *Store) LoadEnabled(n int) []bool {
	en := make([]bool, n)
	for i := range en {
		en[i] = true
	}
	data, err := os.ReadFile(filepath.Join(s.dir, stateFile))
	if err != nil {
		return en
	}
	var st persistedState
	if json.Unmarshal(data, &st) != nil {
		return en
	}
	for i := 0; i < n && i < len(st.Ports); i++ {
		en[i] = st.Ports[i].Enabled
	}
	return en
}

// SaveEnabled persists the enable flags.
func (s *Store) SaveEnabled(en []bool) error {
	var st persistedState
	st.Ports = make([]struct {
		Enabled bool `json:"enabled"`
	}, len(en))
	for i, e := range en {
		st.Ports[i].Enabled = e
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(filepath.Join(s.dir, stateFile), data, 0o644)
}

// AppendSession records a finished session.
func (s *Store) AppendSession(sess Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recent = append(s.recent, sess)
	if len(s.recent) > maxRecentSessions {
		s.recent = s.recent[1:]
	}
	line, err := json.Marshal(sess)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(s.dir, sessionsFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// Recent returns up to limit finished sessions, newest first.
func (s *Store) Recent(limit int) []Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > len(s.recent) {
		limit = len(s.recent)
	}
	out := make([]Session, 0, limit)
	for i := len(s.recent) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, s.recent[i])
	}
	return out
}
