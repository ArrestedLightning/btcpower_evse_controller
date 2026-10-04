package device

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"go.bug.st/serial"

	"github.com/ArrestedLightning/btcpower_evse_controller/internal/proto"
)

// Gap between consecutive commands to a board; the vendor software never sent
// commands back to back.
const cmdGap = 100 * time.Millisecond

// dutyRefresh is how often the duty command is re-sent even if unchanged.
const dutyRefresh = 10 * time.Second

// versionAttempts is how many times the firmware version is requested.
const versionAttempts = 5

func openPort(path string, baud int) (serial.Port, error) {
	p, err := serial.Open(path, &serial.Mode{
		BaudRate: baud,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
	})
	if err != nil {
		return nil, err
	}
	if err := p.SetReadTimeout(200 * time.Millisecond); err != nil {
		p.Close()
		return nil, err
	}
	return p, nil
}

// readLoop reads from port and calls handle with each chunk until the port
// fails or ctx is cancelled.
func readLoop(ctx context.Context, port io.Reader, handle func([]byte)) error {
	buf := make([]byte, 256)
	for ctx.Err() == nil {
		n, err := port.Read(buf)
		if n > 0 {
			handle(buf[:n])
		}
		if err != nil {
			return err
		}
	}
	return ctx.Err()
}

// SerialCharger drives a charge board over a serial port.
type SerialCharger struct {
	Path        string
	Interval    time.Duration
	DebugSerial bool
	Log         *slog.Logger

	mu       sync.Mutex
	reading  ChargerReading
	enable   bool
	duty     int
	lastRx   time.Time
	versionQ atomic.Bool
}

// NewSerialCharger creates a driver; charging is disabled until SetControl.
func NewSerialCharger(path string, interval time.Duration, debug bool, log *slog.Logger) *SerialCharger {
	return &SerialCharger{Path: path, Interval: interval, DebugSerial: debug, Log: log.With("charger", path)}
}

func (c *SerialCharger) Reading() ChargerReading {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reading
}

func (c *SerialCharger) SetControl(enable bool, duty int) {
	c.mu.Lock()
	c.enable, c.duty = enable, duty
	c.mu.Unlock()
}

func (c *SerialCharger) setErr(err error) {
	c.mu.Lock()
	c.reading.Err = err.Error()
	c.mu.Unlock()
}

func (c *SerialCharger) Run(ctx context.Context) {
	for ctx.Err() == nil {
		port, err := openPort(c.Path, proto.ChargerBaud)
		if err != nil {
			c.Log.Warn("open failed", "err", err)
			c.setErr(err)
			sleepCtx(ctx, 5*time.Second)
			continue
		}
		c.Log.Info("port opened")
		err = c.session(ctx, port)
		port.Close()
		if ctx.Err() == nil {
			c.Log.Warn("session ended, reopening", "err", err)
			c.setErr(err)
			sleepCtx(ctx, time.Second)
		}
	}
}

func (c *SerialCharger) session(ctx context.Context, port serial.Port) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c.mu.Lock()
	c.lastRx = time.Now()
	c.mu.Unlock()

	var scanner proto.ChargerScanner
	rxErr := make(chan error, 1)
	go func() {
		rxErr <- readLoop(ctx, port, func(b []byte) {
			if c.DebugSerial {
				c.Log.Debug("rx", "hex", fmt.Sprintf("% X", b))
			}
			scanner.ExpectVersion = c.versionQ.Load()
			for _, m := range scanner.Feed(b) {
				c.handle(m)
			}
		})
	}()

	write := func(b []byte) error {
		if c.DebugSerial {
			c.Log.Debug("tx", "hex", fmt.Sprintf("% X", b))
		}
		_, err := port.Write(b)
		return err
	}

	// Firmware version handshake, as the vendor software did before polling.
	c.versionQ.Store(true)
	for i := 0; i < versionAttempts && c.Reading().Firmware == ""; i++ {
		if err := write(proto.VersionCommand()); err != nil {
			return err
		}
		if !sleepCtx(ctx, 500*time.Millisecond) {
			return ctx.Err()
		}
	}
	c.versionQ.Store(false)

	sentDuty := -1
	var sentAt time.Time
	var sentState uint8 = 255
	ticker := time.NewTicker(c.Interval)
	defer ticker.Stop()
	for {
		c.mu.Lock()
		enable, duty, state, lastRx := c.enable, c.duty, c.reading.Status.State, c.lastRx
		c.mu.Unlock()

		if time.Since(lastRx) > ReopenAfter {
			return errors.New("no valid frames received")
		}
		if err := write(proto.EnableCommand(enable)); err != nil {
			return err
		}
		if duty != sentDuty || state != sentState || time.Since(sentAt) >= dutyRefresh {
			if !sleepCtx(ctx, cmdGap) {
				return ctx.Err()
			}
			if err := write(proto.DutyCommand(duty)); err != nil {
				return err
			}
			sentDuty, sentState, sentAt = duty, state, time.Now()
		}
		if !sleepCtx(ctx, cmdGap) {
			return ctx.Err()
		}
		if err := write(proto.PollCommand()); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-rxErr:
			return err
		case <-ticker.C:
		}
	}
}

func (c *SerialCharger) handle(m proto.ChargerMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	c.lastRx = now
	if m.Status != nil {
		c.reading.Status = *m.Status
		c.reading.Updated = now
		c.reading.Err = ""
	}
	if m.Version != "" {
		c.Log.Info("firmware version", "version", m.Version, "raw", fmt.Sprintf("% X", m.Raw))
		c.reading.Firmware = m.Version
	}
}

// SerialMeter drives a power meter board over a serial port.
type SerialMeter struct {
	Path        string
	DebugSerial bool
	Log         *slog.Logger

	mu      sync.Mutex
	reading MeterReading
}

// NewSerialMeter creates a meter driver.
func NewSerialMeter(path string, debug bool, log *slog.Logger) *SerialMeter {
	return &SerialMeter{Path: path, DebugSerial: debug, Log: log.With("meter", path)}
}

func (m *SerialMeter) Reading() MeterReading {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reading
}

func (m *SerialMeter) Run(ctx context.Context) {
	for ctx.Err() == nil {
		port, err := openPort(m.Path, proto.MeterBaud)
		if err != nil {
			m.Log.Warn("open failed", "err", err)
			m.setErr(err)
			sleepCtx(ctx, 5*time.Second)
			continue
		}
		m.Log.Info("port opened")
		err = m.session(ctx, port)
		port.Close()
		if ctx.Err() == nil {
			m.Log.Warn("session ended, reopening", "err", err)
			m.setErr(err)
			sleepCtx(ctx, time.Second)
		}
	}
}

func (m *SerialMeter) setErr(err error) {
	m.mu.Lock()
	m.reading.Err = err.Error()
	m.mu.Unlock()
}

func (m *SerialMeter) session(ctx context.Context, port serial.Port) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var scanner proto.MeterScanner
	var lastRx atomic.Int64
	lastRx.Store(time.Now().UnixNano())
	rxErr := make(chan error, 1)
	go func() {
		rxErr <- readLoop(ctx, port, func(b []byte) {
			if m.DebugSerial {
				m.Log.Debug("rx", "hex", fmt.Sprintf("% X", b))
			}
			for _, msg := range scanner.Feed(b) {
				now := time.Now()
				lastRx.Store(now.UnixNano())
				m.mu.Lock()
				m.reading = MeterReading{Frame: msg.Frame, Updated: now}
				m.mu.Unlock()
			}
		})
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if time.Since(time.Unix(0, lastRx.Load())) > ReopenAfter {
			return errors.New("no valid frames received")
		}
		if m.DebugSerial {
			m.Log.Debug("tx", "hex", fmt.Sprintf("% X", proto.MeterPollCommand))
		}
		if _, err := port.Write(proto.MeterPollCommand); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-rxErr:
			return err
		case <-ticker.C:
		}
	}
}
