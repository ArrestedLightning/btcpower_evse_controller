// Package config loads, validates and saves the controller configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/ArrestedLightning/btcpower_evse_controller/internal/proto"
)

// Limits on the supported hardware configurations.
const (
	MaxChargers = 4
	MaxMeters   = 2
)

// Current limits derived from the duty cycle range the boards accept.
var (
	MinPortAmps = proto.AmpsForDuty(proto.MinDuty) // 12 A
	MaxPortAmps = proto.AmpsForDuty(proto.MaxDuty) // 75 A
)

// Config is the top-level configuration file.
type Config struct {
	StationName    string      `yaml:"station_name" json:"station_name"`
	Listen         string      `yaml:"listen" json:"listen"`
	AdminPassword  string      `yaml:"admin_password" json:"admin_password,omitempty"`
	DataDir        string      `yaml:"data_dir" json:"data_dir"`
	PollIntervalMS int         `yaml:"poll_interval_ms" json:"poll_interval_ms"`
	Chargers       []Charger   `yaml:"chargers" json:"chargers"`
	Meters         []Meter     `yaml:"meters" json:"meters"`
	LoadSharing    LoadSharing `yaml:"load_sharing" json:"load_sharing"`
}

// Charger is one J1772 port and its control board.
type Charger struct {
	Name    string    `yaml:"name" json:"name"`
	Serial  string    `yaml:"serial" json:"serial"`
	MaxAmps float64   `yaml:"max_amps" json:"max_amps"`
	Meter   *MeterRef `yaml:"meter,omitempty" json:"meter,omitempty"`
}

// MeterRef selects a power meter channel (both numbers are 1-based).
type MeterRef struct {
	Meter   int `yaml:"meter" json:"meter"`
	Channel int `yaml:"channel" json:"channel"`
}

// Meter is a two-channel power meter board.
type Meter struct {
	Name   string `yaml:"name" json:"name"`
	Serial string `yaml:"serial" json:"serial"`
}

// LoadSharing configures the system-wide current budget.
type LoadSharing struct {
	Enabled       bool    `yaml:"enabled" json:"enabled"`
	SystemMaxAmps float64 `yaml:"system_max_amps" json:"system_max_amps"`
	// MinAmps is the lowest current offered to a charging vehicle; ports that
	// cannot get at least this much are paused.
	MinAmps float64 `yaml:"min_amps" json:"min_amps"`
	// Dynamic enables meter-based reallocation of unused current.
	Dynamic bool `yaml:"dynamic" json:"dynamic"`
	// ThrottleMarginAmps: a port drawing this much below its allocation is
	// considered under-using it.
	ThrottleMarginAmps float64 `yaml:"throttle_margin_amps" json:"throttle_margin_amps"`
	// ThrottleHeadroomAmps is left above the measured draw when throttling.
	ThrottleHeadroomAmps float64 `yaml:"throttle_headroom_amps" json:"throttle_headroom_amps"`
	// ThrottleDelaySec is how long under-use must persist before throttling.
	ThrottleDelaySec int `yaml:"throttle_delay_sec" json:"throttle_delay_sec"`
	// ProbeIntervalSec is how often throttled ports are offered their full share again.
	ProbeIntervalSec int `yaml:"probe_interval_sec" json:"probe_interval_sec"`
}

// Default returns a configuration with defaults applied and no ports.
func Default() *Config {
	c := &Config{}
	c.ApplyDefaults()
	return c
}

// ApplyDefaults fills unset fields with default values.
func (c *Config) ApplyDefaults() {
	if c.StationName == "" {
		c.StationName = "EV Charging"
	}
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.DataDir == "" {
		c.DataDir = "/var/lib/evse-controller"
	}
	if c.PollIntervalMS == 0 {
		c.PollIntervalMS = 1000
	}
	for i := range c.Chargers {
		if c.Chargers[i].Name == "" {
			c.Chargers[i].Name = fmt.Sprintf("Port %d", i+1)
		}
	}
	for i := range c.Meters {
		if c.Meters[i].Name == "" {
			c.Meters[i].Name = fmt.Sprintf("Meter %d", i+1)
		}
	}
	ls := &c.LoadSharing
	if ls.MinAmps == 0 {
		ls.MinAmps = MinPortAmps
	}
	if ls.ThrottleMarginAmps == 0 {
		ls.ThrottleMarginAmps = 2
	}
	if ls.ThrottleHeadroomAmps == 0 {
		ls.ThrottleHeadroomAmps = 3
	}
	if ls.ThrottleDelaySec == 0 {
		ls.ThrottleDelaySec = 30
	}
	if ls.ProbeIntervalSec == 0 {
		ls.ProbeIntervalSec = 300
	}
}

// Validate checks the configuration for consistency.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if n := len(c.Chargers); n < 1 || n > MaxChargers {
		add("need 1-%d chargers, have %d", MaxChargers, n)
	}
	if n := len(c.Meters); n > MaxMeters {
		add("at most %d meters supported, have %d", MaxMeters, n)
	}
	if c.PollIntervalMS < 200 || c.PollIntervalMS > 10000 {
		add("poll_interval_ms must be 200-10000")
	}
	serials := map[string]string{}
	checkSerial := func(owner, s string) {
		if s == "" {
			add("%s: serial port is required", owner)
			return
		}
		if prev, ok := serials[s]; ok {
			add("%s: serial port %s already used by %s", owner, s, prev)
		}
		serials[s] = owner
	}
	channels := map[MeterRef]string{}
	for i, ch := range c.Chargers {
		owner := fmt.Sprintf("charger %d (%s)", i+1, ch.Name)
		checkSerial(owner, ch.Serial)
		if ch.MaxAmps < MinPortAmps || ch.MaxAmps > MaxPortAmps {
			add("%s: max_amps must be %g-%g", owner, MinPortAmps, MaxPortAmps)
		}
		if ch.Meter != nil {
			r := *ch.Meter
			if r.Meter < 1 || r.Meter > len(c.Meters) {
				add("%s: meter %d is not configured", owner, r.Meter)
			}
			if r.Channel != 1 && r.Channel != 2 {
				add("%s: meter channel must be 1 or 2", owner)
			}
			if prev, ok := channels[r]; ok {
				add("%s: meter %d channel %d already used by %s", owner, r.Meter, r.Channel, prev)
			}
			channels[r] = owner
		}
	}
	for i, m := range c.Meters {
		checkSerial(fmt.Sprintf("meter %d (%s)", i+1, m.Name), m.Serial)
	}
	ls := c.LoadSharing
	if ls.MinAmps < MinPortAmps {
		add("load_sharing.min_amps must be at least %g", MinPortAmps)
	}
	if ls.Enabled {
		if ls.SystemMaxAmps < ls.MinAmps {
			add("load_sharing.system_max_amps must be at least min_amps (%g)", ls.MinAmps)
		}
		if ls.ThrottleMarginAmps < 0 || ls.ThrottleHeadroomAmps < 1 {
			add("load_sharing throttle margin must be >= 0 and headroom >= 1")
		}
		if ls.ThrottleDelaySec < 5 || ls.ProbeIntervalSec < 30 {
			add("load_sharing throttle_delay_sec must be >= 5 and probe_interval_sec >= 30")
		}
	}
	return errors.Join(errs...)
}

// DynamicAvailable reports whether any port has a meter channel, which is
// required for dynamic load sharing.
func (c *Config) DynamicAvailable() bool {
	for _, ch := range c.Chargers {
		if ch.Meter != nil {
			return true
		}
	}
	return false
}

// Clone returns a deep copy.
func (c *Config) Clone() *Config {
	n := *c
	n.Chargers = make([]Charger, len(c.Chargers))
	for i, ch := range c.Chargers {
		n.Chargers[i] = ch
		if ch.Meter != nil {
			r := *ch.Meter
			n.Chargers[i].Meter = &r
		}
	}
	n.Meters = append([]Meter(nil), c.Meters...)
	return &n
}

// Load reads, defaults and validates a configuration file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Config{}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return c, nil
}

// Save atomically writes the configuration to path with mode 0600, since it
// contains the admin password.
func (c *Config) Save(path string) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, data, 0o600)
}

// WriteFileAtomic writes data to a temporary file and renames it over path.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), perm); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
