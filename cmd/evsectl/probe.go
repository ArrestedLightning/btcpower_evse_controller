package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"go.bug.st/serial"

	"github.com/ArrestedLightning/btcpower_evse_controller/internal/proto"
)

// probeMain talks directly to one board and prints raw and decoded traffic,
// for confirming protocol details on real hardware.
func probeMain(args []string) int {
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	port := fs.String("port", "", "serial device, e.g. /dev/ttyS1 (required)")
	kind := fs.String("type", "charger", "board type: charger or meter")
	count := fs.Int("count", 10, "number of poll cycles (0 = until interrupted)")
	interval := fs.Duration("interval", time.Second, "time between polls")
	enable := fs.Bool("enable", false, "charger: send enable (authorize) command")
	disable := fs.Bool("disable", false, "charger: send disable command")
	keepalive := fs.Bool("keepalive", true, "charger: repeat -enable/-disable every cycle (set false to test for a watchdog)")
	duty := fs.Int("duty", 0, "charger: send this pilot duty cycle (20-94) once at start")
	reset := fs.Bool("reset", false, "charger: send the board reset command (0x7D) at start")
	asJSON := fs.Bool("json", false, "print decoded frames as JSON")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s probe -port /dev/ttyS1 [-type charger|meter] [flags]\n\n"+
			"Stop the controller service first; only one program can use a serial port.\n\n", os.Args[0])
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if *port == "" || (*kind != "charger" && *kind != "meter") || (*enable && *disable) {
		fs.Usage()
		return 2
	}

	baud := proto.ChargerBaud
	if *kind == "meter" {
		baud = proto.MeterBaud
	}
	p, err := serial.Open(*port, &serial.Mode{BaudRate: baud, DataBits: 8, Parity: serial.NoParity, StopBits: serial.OneStopBit})
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		return 1
	}
	defer p.Close()
	p.SetReadTimeout(100 * time.Millisecond)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	start := time.Now()
	stamp := func() string { return fmt.Sprintf("%7.3fs", time.Since(start).Seconds()) }
	send := func(label string, b []byte) {
		fmt.Printf("%s TX %-8s % X\n", stamp(), label, b)
		if _, err := p.Write(b); err != nil {
			fmt.Fprintln(os.Stderr, "write:", err)
		}
	}
	show := func(v any) {
		if *asJSON {
			b, _ := json.Marshal(v)
			fmt.Printf("%s    %s\n", stamp(), b)
		} else {
			fmt.Printf("%s    %+v\n", stamp(), v)
		}
	}

	var cs proto.ChargerScanner
	var ms proto.MeterScanner
	// listen reads for d, printing raw bytes and any decoded frames.
	listen := func(d time.Duration) {
		buf := make([]byte, 256)
		deadline := time.Now().Add(d)
		for time.Now().Before(deadline) && ctx.Err() == nil {
			n, err := p.Read(buf)
			if err != nil {
				fmt.Fprintln(os.Stderr, "read:", err)
				return
			}
			if n == 0 {
				continue
			}
			fmt.Printf("%s RX          % X\n", stamp(), buf[:n])
			if *kind == "meter" {
				for _, m := range ms.Feed(buf[:n]) {
					fmt.Printf("%s    meter frame, header bytes % X\n", stamp(), m.Raw[:3])
					show(m.Frame)
					for i, c := range m.Frame.Channels {
						fmt.Printf("%s    ch%d: %.2f V  %.3f A  %.1f W  %.3f kWh  PF raw %d  freq raw %d\n",
							stamp(), i+1, c.Voltage, c.Current, c.ActivePower, c.EnergyKWh(), c.PowerFactorRaw, c.FrequencyRaw)
					}
				}
				continue
			}
			for _, m := range cs.Feed(buf[:n]) {
				switch {
				case m.Version != "":
					fmt.Printf("%s    firmware version %q (raw % X)\n", stamp(), m.Version, m.Raw)
				case m.Status != nil:
					s := *m.Status
					fmt.Printf("%s    state %d (%s)  pilot %d  duty %d%% (%.1f A)  fault %d  contactor %v  enabled %v\n",
						stamp(), s.State, proto.ClassifyState(s.State), s.PilotState, s.DutyCycle,
						proto.AmpsForDuty(int(s.DutyCycle)), s.FaultCode, s.Contactor, s.Enabled)
					show(s)
				}
			}
		}
	}

	if *kind == "charger" {
		if *reset {
			send("reset", proto.ResetCommand())
			listen(2 * time.Second)
		}
		cs.ExpectVersion = true
		send("version", proto.VersionCommand())
		listen(time.Second)
		cs.ExpectVersion = false
		if *duty != 0 {
			send("duty", proto.DutyCommand(*duty))
			listen(200 * time.Millisecond)
		}
	}

	for i := 0; (*count == 0 || i < *count) && ctx.Err() == nil; i++ {
		if *kind == "meter" {
			send("MC8", proto.MeterPollCommand)
		} else {
			if (*enable || *disable) && (*keepalive || i == 0) {
				send("enable", proto.EnableCommand(*enable))
				listen(100 * time.Millisecond)
			}
			send("poll", proto.PollCommand())
		}
		listen(*interval)
	}
	return 0
}
