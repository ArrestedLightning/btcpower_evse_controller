//go:build linux

package device

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ArrestedLightning/btcpower_evse_controller/internal/proto"
)

// openPTY returns the master side and the slave device path.
func openPTY(t *testing.T) (*os.File, string) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skip("no pty support:", err)
	}
	if err := unix.IoctlSetPointerInt(int(m.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(int(m.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	// Raw mode on the master so bytes pass through unchanged.
	tio, err := unix.IoctlGetTermios(int(m.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	cfmakeraw(tio)
	if err := unix.IoctlSetTermios(int(m.Fd()), unix.TCSETS, tio); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m, fmt.Sprintf("/dev/pts/%d", n)
}

func cfmakeraw(t *unix.Termios) {
	t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	t.Oflag &^= unix.OPOST
	t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	t.Cflag &^= unix.CSIZE | unix.PARENB
	t.Cflag |= unix.CS8
}

// fakeBoard answers charger commands on the pty master and records what it
// received.
type fakeBoard struct {
	mu      sync.Mutex
	rx      []byte
	enabled bool
	duty    byte
}

func (b *fakeBoard) serve(m io.ReadWriter) {
	buf := make([]byte, 64)
	var pending []byte
	for {
		n, err := m.Read(buf)
		if err != nil {
			return
		}
		b.mu.Lock()
		b.rx = append(b.rx, buf[:n]...)
		b.mu.Unlock()
		pending = append(pending, buf[:n]...)
		for {
			i := bytes.IndexByte(pending, proto.ETX)
			if i < 0 || i+1 >= len(pending) {
				break
			}
			frame := pending[:i+2]
			pending = pending[i+2:]
			s := bytes.IndexByte(frame, proto.STX)
			if s < 0 || s+1 >= len(frame) {
				continue
			}
			cmd, data := frame[s+1], frame[s+2:len(frame)-2]
			b.mu.Lock()
			switch cmd {
			case proto.CmdVersion:
				m.Write([]byte("FV4.08..RR"))
			case proto.CmdEnable:
				b.enabled = len(data) == 1 && data[0] == 1
			case proto.CmdSetDuty:
				if len(data) == 1 {
					b.duty = data[0]
				}
			case proto.CmdPoll:
				m.Write(proto.EncodeStatus(proto.Status{State: 4, DutyCycle: b.duty, Enabled: b.enabled, Contactor: true}))
			}
			b.mu.Unlock()
		}
	}
}

func TestSerialChargerAgainstFakeBoard(t *testing.T) {
	master, slave := openPTY(t)
	board := &fakeBoard{}
	go board.serve(master)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := NewSerialCharger(slave, 200*time.Millisecond, false, log)
	c.SetControl(true, 40)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r := c.Reading()
		if r.Fresh(time.Now()) && r.Status.Enabled && r.Status.DutyCycle == 40 {
			if r.Firmware != "4.08." {
				t.Errorf("firmware %q", r.Firmware)
			}
			board.mu.Lock()
			rx := append([]byte(nil), board.rx...)
			board.mu.Unlock()
			for _, want := range [][]byte{proto.VersionCommand(), proto.EnableCommand(true), proto.DutyCommand(40), proto.PollCommand()} {
				if !bytes.Contains(rx, want) {
					t.Errorf("board never received % X", want)
				}
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no enabled status with duty 40; last reading %+v", c.Reading())
}

func TestSerialMeterAgainstFakeBoard(t *testing.T) {
	master, slave := openPTY(t)
	var frame proto.MeterFrame
	frame.Channels[0] = proto.MeterChannel{Voltage: 241.5, Current: 16.25, EnergyRaw: 1234567}
	go func() {
		buf := make([]byte, 64)
		var pending []byte
		for {
			n, err := master.Read(buf)
			if err != nil {
				return
			}
			pending = append(pending, buf[:n]...)
			for {
				i := bytes.Index(pending, proto.MeterPollCommand)
				if i < 0 {
					break
				}
				pending = pending[i+len(proto.MeterPollCommand):]
				// Reply in two chunks to exercise reassembly.
				raw := proto.EncodeMeter(frame)
				master.Write(raw[:40])
				time.Sleep(20 * time.Millisecond)
				master.Write(raw[40:])
			}
		}
	}()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := NewSerialMeter(slave, false, log)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if r := m.Reading(); r.Fresh(time.Now()) {
			if r.Frame.Channels[0] != frame.Channels[0] {
				t.Fatalf("channel 1 = %+v", r.Frame.Channels[0])
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no meter reading; last %+v", m.Reading())
}
