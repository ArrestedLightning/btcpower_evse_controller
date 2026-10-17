package proto

import (
	"bytes"
	"testing"
)

// sampleStatus is the captured frame from the vendor's chargerData.txt.
var sampleStatus = []byte{
	0x53, 0x54, 0x41, 0x02, 0x00, 0x00, 0x3C, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00,
	0x01, 0x00, 0x00, 0x03, 0xFF, 0x03, 0xC6, 0xFF, 0x7F, 0x00, 0x7E, 0x45, 0x4E, 0x44,
}

func TestCommandsMatchVendorConstants(t *testing.T) {
	cases := []struct {
		name string
		got  []byte
		want []byte
	}{
		{"poll", PollCommand(), []byte{0x02, 0x40, 0x03, 0x41}},
		{"version", VersionCommand(), []byte{0x02, 0x41, 0x03, 0x40}},
		{"disable", EnableCommand(false), []byte{0x02, 0x39, 0x03, 0x38}},
		{"enable", EnableCommand(true), []byte{0x02, 0x39, 0x01, 0x03, 0x39}},
		{"reset", ResetCommand(), []byte{0x02, 0x7D, 0x03, 0x7C}},
		{"duty50", DutyCommand(50), []byte{0x02, 0x33, 50, 0x03, 0x02 ^ 0x33 ^ 50 ^ 0x03}},
		{"dutyClampLow", DutyCommand(5), []byte{0x02, 0x33, 20, 0x03, 0x02 ^ 0x33 ^ 20 ^ 0x03}},
		{"dutyClampHigh", DutyCommand(99), []byte{0x02, 0x33, 94, 0x03, 0x02 ^ 0x33 ^ 94 ^ 0x03}},
	}
	for _, c := range cases {
		if !bytes.Equal(c.got, c.want) {
			t.Errorf("%s: got % X, want % X", c.name, c.got, c.want)
		}
	}
}

func TestParseSampleStatus(t *testing.T) {
	s, err := ParseStatus(sampleStatus)
	if err != nil {
		t.Fatal(err)
	}
	if s.State != 2 || s.DutyCycle != 60 || !s.V110Detect || !s.Enabled || s.Contactor {
		t.Errorf("unexpected decode: %+v", s)
	}
	if s.RawPilotPlus != 0x03FF || s.RawTemperature != 0x03C6 {
		t.Errorf("unexpected ADC values: %+v", s)
	}
	if s.PilotVoltsPlus != -12.9 || s.PilotVoltsMin != 12.6 {
		t.Errorf("pilot volts %v %v", s.PilotVoltsPlus, s.PilotVoltsMin)
	}
	if !bytes.Equal(EncodeStatus(s), sampleStatus) {
		t.Errorf("round trip mismatch:\n% X\n% X", EncodeStatus(s), sampleStatus)
	}
}

func TestChargerScannerResync(t *testing.T) {
	var sc ChargerScanner
	stream := append([]byte{0x00, 'S', 'T', 0x11}, sampleStatus[:10]...)
	if msgs := sc.Feed(stream); len(msgs) != 0 {
		t.Fatalf("got %d frames from partial data", len(msgs))
	}
	msgs := sc.Feed(append(append([]byte(nil), sampleStatus[10:]...), sampleStatus...))
	if len(msgs) != 2 {
		t.Fatalf("got %d frames, want 2", len(msgs))
	}
	// Corrupt trailer must be rejected, then the next frame found.
	bad := append([]byte(nil), sampleStatus...)
	bad[27] = 'X'
	msgs = sc.Feed(append(bad, sampleStatus...))
	if len(msgs) != 1 {
		t.Fatalf("got %d frames after corrupt frame, want 1", len(msgs))
	}
}

func TestChargerScannerVersion(t *testing.T) {
	sc := ChargerScanner{ExpectVersion: true}
	reply := []byte("FV4.08..R")
	reply = append(reply, 'R') // 'R' must be at index >= 9
	msgs := sc.Feed(reply[:5])
	if len(msgs) != 0 {
		t.Fatal("frame from partial version reply")
	}
	msgs = sc.Feed(reply[5:])
	if len(msgs) != 1 || msgs[0].Version != "4.08." {
		t.Fatalf("version msgs = %+v", msgs)
	}
}

func TestDutyConversion(t *testing.T) {
	cases := []struct {
		amps float64
		duty int
	}{{6, 10}, {12, 20}, {16, 26}, {18, 30}, {30, 50}, {32, 53}, {48, 80}, {51, 85}, {60, 88}, {80, 96}}
	for _, c := range cases {
		if d := DutyForAmps(c.amps); d != c.duty {
			t.Errorf("DutyForAmps(%v) = %d, want %d", c.amps, d, c.duty)
		}
		if a := AmpsForDuty(DutyForAmps(c.amps)); a > c.amps+1e-9 {
			t.Errorf("advertised %v exceeds requested %v", a, c.amps)
		}
	}
}

func TestMeterRoundTrip(t *testing.T) {
	in := MeterFrame{Firmware: "1.02", Counter: 7}
	in.Channels[0] = MeterChannel{Voltage: 240.12, Current: 31.5, ActivePower: 7563.78, EnergyRaw: 123456789, EnergyOverflow: 1}
	in.Channels[1] = MeterChannel{Voltage: 239.5, Current: 0.25}
	raw := EncodeMeter(in)
	var sc MeterScanner
	msgs := sc.Feed(append([]byte("junkRxx"), raw...))
	if len(msgs) != 1 {
		t.Fatalf("got %d frames", len(msgs))
	}
	out := msgs[0].Frame
	if out.Channels[0] != in.Channels[0] || out.Channels[1] != in.Channels[1] {
		t.Errorf("channels mismatch: %+v", out.Channels)
	}
	if out.Firmware != "1.02" || out.Counter != 7 {
		t.Errorf("firmware/counter: %q %d", out.Firmware, out.Counter)
	}
	want := (float64(1<<32) + 123456789) / 100000
	if got := out.Channels[0].EnergyKWh(); got != want {
		t.Errorf("energy %v, want %v", got, want)
	}
}

func TestClassifyState(t *testing.T) {
	for code, want := range map[uint8]ChargeState{0: StateIdle, 21: StateIdle, 3: StateConnected, 26: StateConnected, 4: StateCharging, 24: StateCharging, 5: StateFault, 99: StateUnknown} {
		if got := ClassifyState(code); got != want {
			t.Errorf("ClassifyState(%d) = %s, want %s", code, got, want)
		}
	}
}
