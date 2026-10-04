package proto

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// MeterBaud is the serial speed of the power meter board.
const MeterBaud = 9600

// MeterFrameLen is the length of a meter reply: 'R' at [0], payload at
// [3..87], 'E' at [88]. Header bytes [1..2] are not interpreted.
const MeterFrameLen = 89

const (
	meterStart = 'R'
	meterEnd   = 'E'
)

// Meter commands are bare ASCII with no framing.
var (
	MeterPollCommand    = []byte("MC8")
	MeterResetCh1Energy = []byte("MA4")
	MeterResetCh2Energy = []byte("MB4")
)

// MeterChannel holds the readings for one metered port.
type MeterChannel struct {
	Voltage        float64 `json:"voltage"`          // V
	Current        float64 `json:"current"`          // A
	ActivePower    float64 `json:"active_power"`     // W
	ReactivePower  float64 `json:"reactive_power"`   // var (scale assumed same as active)
	ApparentPower  float64 `json:"apparent_power"`   // VA (scale assumed same as active)
	EnergyRaw      uint32  `json:"energy_raw"`       // 0.01 Wh units
	EnergyOverflow uint32  `json:"energy_overflow"`  // assumed count of EnergyRaw rollovers
	PowerFactorRaw uint16  `json:"power_factor_raw"` // scale unknown
	FrequencyRaw   uint16  `json:"frequency_raw"`    // scale unknown
}

// EnergyKWh returns total energy, combining the raw counter and overflow.
func (c MeterChannel) EnergyKWh() float64 {
	total := uint64(c.EnergyOverflow)<<32 | uint64(c.EnergyRaw)
	return float64(total) / 100000
}

// MeterFrame is a decoded power meter reply.
type MeterFrame struct {
	Channels [2]MeterChannel `json:"channels"`
	Firmware string          `json:"firmware"`
	Counter  uint8           `json:"counter"`
}

// ParseMeter decodes an 89-byte meter frame.
func ParseMeter(f []byte) (MeterFrame, error) {
	if len(f) != MeterFrameLen {
		return MeterFrame{}, fmt.Errorf("meter frame length %d, want %d", len(f), MeterFrameLen)
	}
	if f[0] != meterStart || f[MeterFrameLen-1] != meterEnd {
		return MeterFrame{}, fmt.Errorf("meter frame missing R/E markers")
	}
	var m MeterFrame
	m.Channels[0] = parseChannel(f[3:35])
	m.Channels[1] = parseChannel(f[35:67])
	fw := f[67:71]
	if isPrintable(fw) {
		m.Firmware = string(bytes.TrimSpace(fw))
	}
	m.Counter = f[71]
	return m, nil
}

func parseChannel(b []byte) MeterChannel {
	be := binary.BigEndian
	return MeterChannel{
		Voltage:        float64(be.Uint32(b[0:])) / 100,
		Current:        float64(be.Uint32(b[4:])) / 1000,
		ActivePower:    float64(be.Uint32(b[8:])) / 100,
		ReactivePower:  float64(be.Uint32(b[12:])) / 100,
		ApparentPower:  float64(be.Uint32(b[16:])) / 100,
		EnergyRaw:      be.Uint32(b[20:]),
		EnergyOverflow: be.Uint32(b[24:]),
		PowerFactorRaw: be.Uint16(b[28:]),
		FrequencyRaw:   be.Uint16(b[30:]),
	}
}

// EncodeMeter builds a meter frame; used by the simulator and tests.
func EncodeMeter(m MeterFrame) []byte {
	f := make([]byte, MeterFrameLen)
	f[0] = meterStart
	f[MeterFrameLen-1] = meterEnd
	encodeChannel(f[3:35], m.Channels[0])
	encodeChannel(f[35:67], m.Channels[1])
	copy(f[67:71], []byte(fmt.Sprintf("%-4.4s", m.Firmware)))
	f[71] = m.Counter
	return f
}

func encodeChannel(b []byte, c MeterChannel) {
	be := binary.BigEndian
	be.PutUint32(b[0:], uint32(c.Voltage*100+0.5))
	be.PutUint32(b[4:], uint32(c.Current*1000+0.5))
	be.PutUint32(b[8:], uint32(c.ActivePower*100+0.5))
	be.PutUint32(b[12:], uint32(c.ReactivePower*100+0.5))
	be.PutUint32(b[16:], uint32(c.ApparentPower*100+0.5))
	be.PutUint32(b[20:], c.EnergyRaw)
	be.PutUint32(b[24:], c.EnergyOverflow)
	be.PutUint16(b[28:], c.PowerFactorRaw)
	be.PutUint16(b[30:], c.FrequencyRaw)
}

// MeterScanner splits a meter byte stream into frames.
type MeterScanner struct {
	buf []byte
}

// MeterMessage is a frame received from a meter board.
type MeterMessage struct {
	Frame MeterFrame
	Raw   []byte
}

// Feed appends received bytes and returns any complete frames.
func (s *MeterScanner) Feed(data []byte) []MeterMessage {
	s.buf = append(s.buf, data...)
	var out []MeterMessage
	for {
		i := bytes.IndexByte(s.buf, meterStart)
		if i < 0 {
			s.buf = s.buf[:0]
			break
		}
		if len(s.buf)-i < MeterFrameLen {
			s.buf = append(s.buf[:0], s.buf[i:]...)
			break
		}
		frame := s.buf[i : i+MeterFrameLen]
		m, err := ParseMeter(frame)
		if err != nil {
			s.buf = s.buf[i+1:]
			continue
		}
		out = append(out, MeterMessage{Frame: m, Raw: append([]byte(nil), frame...)})
		s.buf = s.buf[i+MeterFrameLen:]
	}
	if len(s.buf) > maxScanBuf {
		s.buf = s.buf[len(s.buf)-maxScanBuf:]
	}
	return out
}

// Reset discards buffered bytes.
func (s *MeterScanner) Reset() { s.buf = s.buf[:0] }
