// Package proto implements the serial wire protocols used by the BTC Power
// charge control boards and power meter board.
//
// The protocols were reverse engineered from the original vendor software
// (linuxPOS/serialChargerInterface/uartCharger1.c and uartPowerMeter.c).
package proto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Charge board framing bytes and command opcodes.
const (
	STX = 0x02
	ETX = 0x03

	CmdSetDuty = 0x33 // data: pilot PWM duty cycle in percent
	CmdEnable  = 0x39 // data: 0x01 = enable; no data = disable
	CmdPoll    = 0x40 // request status frame
	CmdVersion = 0x41 // request firmware version
	CmdReset   = 0x7D // reset board
)

// Duty cycle limits accepted by the original software for CmdSetDuty.
const (
	MinDuty = 20
	MaxDuty = 94
)

// ChargerBaud is the serial speed of the charge control boards.
const ChargerBaud = 38400

// StatusFrameLen is the length of a charge board status frame ("STA" ... "END").
const StatusFrameLen = 29

var (
	statusHeader  = []byte("STA")
	statusTrailer = []byte("END")
)

// Command builds a charge board command frame:
// STX CMD [DATA...] ETX XOR, where XOR covers every preceding byte.
func Command(cmd byte, data ...byte) []byte {
	f := make([]byte, 0, len(data)+4)
	f = append(f, STX, cmd)
	f = append(f, data...)
	f = append(f, ETX)
	var x byte
	for _, b := range f {
		x ^= b
	}
	return append(f, x)
}

// PollCommand requests a status frame.
func PollCommand() []byte { return Command(CmdPoll) }

// VersionCommand requests the firmware version.
func VersionCommand() []byte { return Command(CmdVersion) }

// ResetCommand resets the board.
func ResetCommand() []byte { return Command(CmdReset) }

// EnableCommand authorizes (true) or de-authorizes (false) charging.
func EnableCommand(enable bool) []byte {
	if enable {
		return Command(CmdEnable, 0x01)
	}
	return Command(CmdEnable)
}

// DutyCommand sets the pilot PWM duty cycle in percent. The duty is clamped
// to [MinDuty, MaxDuty].
func DutyCommand(duty int) []byte {
	if duty < MinDuty {
		duty = MinDuty
	}
	if duty > MaxDuty {
		duty = MaxDuty
	}
	return Command(CmdSetDuty, byte(duty))
}

// DutyForAmps converts a current limit to the J1772 pilot duty cycle,
// rounding down so the advertised current never exceeds amps.
func DutyForAmps(amps float64) int {
	const eps = 1e-9
	if amps <= 51 {
		return int(math.Floor(amps/0.6 + eps))
	}
	return int(math.Floor(amps/2.5 + 64 + eps))
}

// AmpsForDuty converts a J1772 pilot duty cycle to the advertised current.
func AmpsForDuty(duty int) float64 {
	switch {
	case duty < 8:
		return 0
	case duty < 10:
		return 6
	case duty <= 85:
		return float64(duty) * 0.6
	case duty <= 96:
		return float64(duty-64) * 2.5
	default:
		return 0
	}
}

// Status is a decoded charge board status frame.
type Status struct {
	State          uint8   `json:"state"`
	PilotState     uint8   `json:"pilot_state"`
	AutoMode       uint8   `json:"auto_mode"`
	DutyCycle      uint8   `json:"duty_cycle"`
	FaultCode      uint8   `json:"fault_code"`
	LeakDetect     bool    `json:"leak_detect"`
	ACOutPresent   bool    `json:"ac_out_present"`
	V110Detect     bool    `json:"v110_detect"`
	GFCIReset      bool    `json:"gfci_reset"`
	GFCITest       bool    `json:"gfci_test"`
	Contactor      bool    `json:"contactor"`
	MinusVoltageOK bool    `json:"minus_voltage_ok"`
	Enabled        bool    `json:"enabled"`
	RawPilotMinus  uint16  `json:"raw_pilot_adc_minus"`
	RawPilotPlus   uint16  `json:"raw_pilot_adc_plus"`
	RawTemperature uint16  `json:"raw_temperature_adc"`
	PilotVoltsPlus float64 `json:"pilot_volts_plus"`
	PilotVoltsMin  float64 `json:"pilot_volts_minus"`
}

// ParseStatus decodes a 29-byte status frame.
func ParseStatus(f []byte) (Status, error) {
	if len(f) != StatusFrameLen {
		return Status{}, fmt.Errorf("status frame length %d, want %d", len(f), StatusFrameLen)
	}
	if !bytes.Equal(f[:3], statusHeader) || !bytes.Equal(f[26:], statusTrailer) {
		return Status{}, errors.New("status frame missing STA/END markers")
	}
	be := binary.BigEndian
	return Status{
		State:          f[3],
		PilotState:     f[4],
		AutoMode:       f[5],
		DutyCycle:      f[6],
		FaultCode:      f[7],
		LeakDetect:     f[8] != 0,
		ACOutPresent:   f[9] != 0,
		V110Detect:     f[10] != 0,
		GFCIReset:      f[11] != 0,
		GFCITest:       f[12] != 0,
		Contactor:      f[13] != 0,
		MinusVoltageOK: f[14] != 0,
		Enabled:        f[15] != 0,
		RawPilotMinus:  be.Uint16(f[16:]),
		RawPilotPlus:   be.Uint16(f[18:]),
		RawTemperature: be.Uint16(f[20:]),
		PilotVoltsPlus: float64(int16(be.Uint16(f[22:]))) / 10,
		PilotVoltsMin:  float64(int16(be.Uint16(f[24:]))) / 10,
	}, nil
}

// EncodeStatus builds a status frame; used by the simulator and tests.
func EncodeStatus(s Status) []byte {
	f := make([]byte, StatusFrameLen)
	copy(f, statusHeader)
	copy(f[26:], statusTrailer)
	b := func(v bool) byte {
		if v {
			return 1
		}
		return 0
	}
	f[3], f[4], f[5], f[6], f[7] = s.State, s.PilotState, s.AutoMode, s.DutyCycle, s.FaultCode
	f[8], f[9], f[10], f[11] = b(s.LeakDetect), b(s.ACOutPresent), b(s.V110Detect), b(s.GFCIReset)
	f[12], f[13], f[14], f[15] = b(s.GFCITest), b(s.Contactor), b(s.MinusVoltageOK), b(s.Enabled)
	be := binary.BigEndian
	be.PutUint16(f[16:], s.RawPilotMinus)
	be.PutUint16(f[18:], s.RawPilotPlus)
	be.PutUint16(f[20:], s.RawTemperature)
	be.PutUint16(f[22:], uint16(int16(math.Round(s.PilotVoltsPlus*10))))
	be.PutUint16(f[24:], uint16(int16(math.Round(s.PilotVoltsMin*10))))
	return f
}

// ChargeState is the high-level interpretation of a board State code.
type ChargeState string

const (
	StateIdle      ChargeState = "idle"
	StateConnected ChargeState = "connected"
	StateCharging  ChargeState = "charging"
	StateFault     ChargeState = "fault"
	StateUnknown   ChargeState = "unknown"
	StateOffline   ChargeState = "offline"
)

// ClassifyState maps a raw board State code to a ChargeState. The grouping
// follows the vendor kiosk UI (POSGUI2.php); codes 21-26 are undocumented.
func ClassifyState(code uint8) ChargeState {
	switch code {
	case 0, 1, 2, 21:
		return StateIdle
	case 3, 6, 22, 25, 26:
		return StateConnected
	case 4, 23, 24:
		return StateCharging
	case 5:
		return StateFault
	default:
		return StateUnknown
	}
}

// ChargerMessage is a frame received from a charge board.
type ChargerMessage struct {
	Status  *Status // set for status frames
	Version string  // set for firmware version replies
	Raw     []byte
}

// ChargerScanner splits a charge board byte stream into frames.
type ChargerScanner struct {
	buf []byte
	// ExpectVersion enables recognition of firmware version replies
	// ('F' ... 'R'), which carry no unambiguous header.
	ExpectVersion bool
}

const (
	maxScanBuf    = 512
	versionMinEnd = 9
	versionMaxEnd = 24
)

// Feed appends received bytes and returns any complete frames.
func (s *ChargerScanner) Feed(data []byte) []ChargerMessage {
	s.buf = append(s.buf, data...)
	var out []ChargerMessage
	for {
		i := bytes.Index(s.buf, statusHeader)
		if s.ExpectVersion {
			if j := bytes.IndexByte(s.buf, 'F'); j >= 0 && (i < 0 || j < i) {
				if msg, n, ok := s.scanVersion(j); ok {
					out = append(out, msg)
					s.buf = s.buf[n:]
					continue
				}
				if len(s.buf)-j > versionMaxEnd {
					// Not a version reply; drop the 'F' and rescan.
					s.buf = s.buf[j+1:]
					continue
				}
				if i < 0 {
					s.trim(j) // incomplete reply; wait for more bytes
					break
				}
			}
		}
		if i < 0 {
			// Keep a possible partial header at the end.
			s.trim(len(s.buf) - (len(statusHeader) - 1))
			break
		}
		if len(s.buf)-i < StatusFrameLen {
			s.trim(i)
			break
		}
		frame := s.buf[i : i+StatusFrameLen]
		st, err := ParseStatus(frame)
		if err != nil {
			// False header; skip past it and resync.
			s.buf = s.buf[i+1:]
			continue
		}
		out = append(out, ChargerMessage{Status: &st, Raw: append([]byte(nil), frame...)})
		s.buf = s.buf[i+StatusFrameLen:]
	}
	if len(s.buf) > maxScanBuf {
		s.buf = s.buf[len(s.buf)-maxScanBuf:]
	}
	return out
}

// scanVersion looks for a firmware reply starting at j: 'F' followed by an
// 'R' at offset >= 9 (per the vendor parser); version text is bytes [2..6].
func (s *ChargerScanner) scanVersion(j int) (ChargerMessage, int, bool) {
	for k := j + versionMinEnd; k < len(s.buf) && k <= j+versionMaxEnd; k++ {
		if s.buf[k] == 'R' {
			frame := append([]byte(nil), s.buf[j:k+1]...)
			return ChargerMessage{Version: versionText(frame), Raw: frame}, k + 1, true
		}
	}
	return ChargerMessage{}, 0, false
}

func versionText(frame []byte) string {
	v := frame[2:7]
	if isPrintable(v) {
		return string(bytes.TrimSpace(v))
	}
	var p []byte
	for _, b := range frame {
		if b >= 0x20 && b < 0x7f {
			p = append(p, b)
		}
	}
	return string(p)
}

func isPrintable(b []byte) bool {
	for _, c := range b {
		if c < 0x20 || c >= 0x7f {
			return false
		}
	}
	return true
}

// Reset discards buffered bytes.
func (s *ChargerScanner) Reset() { s.buf = s.buf[:0] }

func (s *ChargerScanner) trim(from int) {
	if from <= 0 {
		return
	}
	s.buf = append(s.buf[:0], s.buf[from:]...)
}
