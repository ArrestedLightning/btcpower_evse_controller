package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func valid() *Config {
	c := &Config{
		Chargers: []Charger{
			{Serial: "/dev/ttyS1", MaxAmps: 32, Meter: &MeterRef{Meter: 1, Channel: 1}},
			{Serial: "/dev/ttyS3", MaxAmps: 32, Meter: &MeterRef{Meter: 1, Channel: 2}},
		},
		Meters:      []Meter{{Serial: "/dev/ttyS2"}},
		LoadSharing: LoadSharing{Enabled: true, SystemMaxAmps: 40, Dynamic: true},
	}
	c.ApplyDefaults()
	return c
}

func TestValidateOK(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateErrors(t *testing.T) {
	cases := map[string]func(c *Config){
		"need 1-4 chargers":       func(c *Config) { c.Chargers = nil },
		"at most 2 meters":        func(c *Config) { c.Meters = append(c.Meters, Meter{Serial: "a"}, Meter{Serial: "b"}) },
		"already used by":         func(c *Config) { c.Chargers[1].Serial = "/dev/ttyS2" },
		"max_amps must be":        func(c *Config) { c.Chargers[0].MaxAmps = 100 },
		"meter 2 is not":          func(c *Config) { c.Chargers[0].Meter.Meter = 2 },
		"channel must be 1 or 2":  func(c *Config) { c.Chargers[0].Meter.Channel = 3 },
		"channel 1 already used":  func(c *Config) { c.Chargers[1].Meter.Channel = 1 },
		"system_max_amps":         func(c *Config) { c.LoadSharing.SystemMaxAmps = 5 },
		"serial port is required": func(c *Config) { c.Chargers[0].Serial = "" },
		"display.theme must be":   func(c *Config) { c.Display.Theme = "blue" },
		"session_end_delay_sec":   func(c *Config) { d := -1; c.SessionEndDelaySec = &d },
	}
	for want, mutate := range cases {
		c := valid()
		mutate(c)
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got error %v", want, err)
		}
	}
}

func TestDisplayDefaults(t *testing.T) {
	c := valid()
	if c.SessionEndDelay() != 10*time.Second {
		t.Fatalf("session end delay default %v", c.SessionEndDelay())
	}
	if c.Display.Theme != ThemeDark || !c.Display.ClockVisible() {
		t.Fatalf("defaults: %+v", c.Display)
	}
	clone := c.Clone()
	*clone.Display.ShowClock = false
	if !c.Display.ClockVisible() {
		t.Fatal("Clone shares ShowClock")
	}
}

func TestFiveChargersRejected(t *testing.T) {
	c := valid()
	for i := 0; i < 3; i++ {
		c.Chargers = append(c.Chargers, Charger{Serial: "/dev/ttyUSB" + string(rune('0'+i)), MaxAmps: 16})
	}
	if err := c.Validate(); err == nil {
		t.Fatal("5 chargers accepted")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	c := valid()
	c.AdminPassword = "secret"
	hide := false
	c.Display = Display{Theme: ThemeLight, ShowClock: &hide}
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Display.Theme != ThemeLight || got.Display.ClockVisible() {
		t.Errorf("display round trip: %+v", got.Display)
	}
	if got.AdminPassword != "secret" || len(got.Chargers) != 2 || got.Chargers[1].Meter.Channel != 2 || got.Chargers[0].Name != "Port 1" {
		t.Errorf("round trip: %+v", got)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(path, []byte("chargers:\n  - serial: /dev/ttyS1\n    max_amp: 32\n"), 0o600)
	if _, err := Load(path); err == nil {
		t.Fatal("typo'd field accepted")
	}
}
