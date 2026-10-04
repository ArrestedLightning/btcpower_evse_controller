// Package device drives the charge control boards and power meters over
// serial, and provides simulated equivalents.
package device

import (
	"context"
	"time"

	"github.com/ArrestedLightning/btcpower_evse_controller/internal/proto"
)

// Freshness thresholds.
const (
	// StaleAfter: data older than this is treated as offline.
	StaleAfter = 5 * time.Second
	// ReopenAfter: with no valid frames for this long the port is reopened.
	ReopenAfter = 30 * time.Second
)

// ChargerReading is the latest information from a charge board.
type ChargerReading struct {
	Status   proto.Status
	Firmware string
	Updated  time.Time // zero if no status received yet
	Err      string    // last open/IO error, if any
}

// Fresh reports whether the reading is recent enough to trust.
func (r ChargerReading) Fresh(now time.Time) bool {
	return !r.Updated.IsZero() && now.Sub(r.Updated) < StaleAfter
}

// Charger is a charge control board.
type Charger interface {
	// Run drives the board until ctx is cancelled.
	Run(ctx context.Context)
	Reading() ChargerReading
	// SetControl sets the desired enable state and pilot duty cycle.
	SetControl(enable bool, duty int)
}

// MeterReading is the latest information from a power meter.
type MeterReading struct {
	Frame   proto.MeterFrame
	Updated time.Time
	Err     string
}

// Fresh reports whether the reading is recent enough to trust.
func (r MeterReading) Fresh(now time.Time) bool {
	return !r.Updated.IsZero() && now.Sub(r.Updated) < StaleAfter
}

// Meter is a two-channel power meter board.
type Meter interface {
	Run(ctx context.Context)
	Reading() MeterReading
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
