package main

import (
	"context"
	"errors"
	"log"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sys/unix"
)

const (
	e450Baud = 2400 // W6832 pass-through, fixed by the meter
	// e450HeadLen is the length of a push head: six 0x12 tags, each
	// followed by a big-endian uint16 value.
	e450HeadLen = 18
	// The meter pushes every ~5 s; main_up decays after one minute
	// without a push.
	e450UpTimeout = 60 * time.Second
	// e450Retry is how long the binary waits before reopening the port.
	e450Retry = 30 * time.Second
)

// e450Phases indexes the per-phase metrics: L1..L3, then L123 for the total.
var e450Phases = []string{"L1", "L2", "L3", "L123"}

// head is the 18-byte head of a CII push frame: the first three values are
// the per-phase currents in mA, the last three the phase-to-neutral
// voltages in V, each big-endian behind a 0x12 tag.
type head struct {
	currentMA [3]uint16
	voltageV  [3]uint16
}

var (
	mainUp = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "main_up",
		Help: "Whether a push from the main meter was received within the last minute",
	})
	mainCurrentA = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "main_current_a",
		Help: "Current measured by the main meter (A), per phase and total",
	}, []string{"phase"})
	mainVoltageV = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "main_voltage_v",
		Help: "Line-to-neutral voltage measured by the main meter (V), per phase",
	}, []string{"phase"})
	mainPowerW = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "main_power_w",
		Help: "Estimated active power of the main meter (W), per phase and total; U x I without power factor",
	}, []string{"phase"})
	mainEnergyWhTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "main_energy_wh_total",
		Help: "Cumulative active energy (Wh), per phase and total, integrated from the push values; resets when the process restarts",
	}, []string{"phase"})
)

func init() {
	for _, c := range []prometheus.Collector{mainUp, mainCurrentA, mainVoltageV, mainPowerW, mainEnergyWhTotal} {
		prometheus.MustRegister(c)
	}
}

// validHead reports whether b looks like a push head: six 0x12 tags at fixed
// positions, with the three voltage values in a plausible phase-to-neutral
// range. The frame bodies of the captures contain no such pattern, so this
// also keeps the resync loop from matching inside a frame.
func validHead(b []byte) bool {
	if len(b) < e450HeadLen {
		return false
	}
	for j := 0; j < 6; j++ {
		if b[3*j] != 0x12 {
			return false
		}
	}
	for j := 0; j < 3; j++ {
		v := uint16(b[3*j+10])<<8 | uint16(b[3*j+11])
		if v < 80 || v > 300 {
			return false
		}
	}
	return true
}

// parseHead decodes the six values of a push head.
func parseHead(b []byte) head {
	var h head
	for j := 0; j < 3; j++ {
		h.currentMA[j] = uint16(b[3*j+1])<<8 | uint16(b[3*j+2])
		h.voltageV[j] = uint16(b[3*j+10])<<8 | uint16(b[3*j+11])
	}
	return h
}

// findPushHead returns the offset of the first valid head in buf, or -1.
func findPushHead(buf []byte) int {
	for i := 0; i+e450HeadLen <= len(buf); i++ {
		if validHead(buf[i:]) {
			return i
		}
	}
	return -1
}

// energyDeltas returns the Wh to add to each phase (L1, L2, L3, L123) for a
// push arriving dt after the previous one: the phase power at this sample
// times dt.
func energyDeltas(h head, dt time.Duration) [4]float64 {
	var d [4]float64
	for j := 0; j < 3; j++ {
		d[j] = float64(h.currentMA[j]) / 1000 * float64(h.voltageV[j]) * dt.Seconds() / 3600
		d[3] += d[j]
	}
	return d
}

// E450 reads the CII push stream of the building's main meter (Landis+Gyr
// E450) from the W6832 M-Bus dongle. The meter pushes a frame every ~5 s;
// the binary picks each head out of the stream, without assuming a fixed
// frame length.
type E450 struct {
	dev string
	// onPush, if set, is called for each valid frame instead of applying
	// it to the metrics. Dump mode uses it to print frames. frame runs
	// from the head to the end of the buffered stream.
	onPush func(h head, frame []byte)

	buf []byte
	// lastPush is written and read only from the Run loop, so no lock is
	// needed.
	lastPush time.Time
}

func NewE450(dev string) *E450 {
	return &E450{dev: dev}
}

// Run reads the CII stream until ctx is done, reopening the port after
// errors (e.g. the dongle being replugged).
func (e *E450) Run(ctx context.Context) {
	wait := func(d time.Duration) bool {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			return true
		}
	}

	var f *os.File
	for {
		e.updateUp()
		if f == nil {
			var err error
			f, err = openE450TTY(e.dev)
			if err != nil {
				log.Printf("e450: open %s: %v", e.dev, err)
				if !wait(e450Retry) {
					return
				}
				continue
			}
			log.Printf("e450: reading %s at %d 8N1", e.dev, e450Baud)
		}
		rd := make([]byte, 512)
		n, err := f.Read(rd)
		if n > 0 {
			e.buf = append(e.buf, rd[:n]...)
			e.process()
		}
		if err == nil {
			continue
		}
		if !errors.Is(err, unix.EAGAIN) {
			log.Printf("e450: read: %v; reopening in %v", err, e450Retry)
			_ = f.Close()
			f = nil
			if !wait(e450Retry) {
				return
			}
			continue
		}
		if !wait(100 * time.Millisecond) { // no data; the push comes every ~5 s
			return
		}
	}
}

// process consumes every complete push head in e.buf, keeping only the
// trailing bytes that could still grow into a head.
func (e *E450) process() {
	for {
		i := findPushHead(e.buf)
		if i < 0 {
			if len(e.buf) > e450HeadLen-1 {
				e.buf = e.buf[len(e.buf)-(e450HeadLen-1):]
			}
			return
		}
		h := parseHead(e.buf[i:])
		if e.onPush != nil {
			e.onPush(h, e.buf[i:])
		} else {
			e.applyPush(h)
		}
		e.buf = e.buf[i+e450HeadLen:]
	}
}

// applyPush publishes one push frame to the main_* metrics and advances
// main_energy_wh_total by the phase power times the time since the
// previous push.
func (e *E450) applyPush(h head) {
	now := time.Now()
	dt := time.Duration(0)
	if !e.lastPush.IsZero() {
		dt = now.Sub(e.lastPush) // monotonic clock; wall-clock steps don't leak in
	}
	e.lastPush = now

	deltas := energyDeltas(h, dt)
	var totalW float64
	for j := 0; j < 3; j++ {
		phase := e450Phases[j]
		w := float64(h.currentMA[j]) / 1000 * float64(h.voltageV[j])
		totalW += w
		mainCurrentA.WithLabelValues(phase).Set(float64(h.currentMA[j]) / 1000)
		mainVoltageV.WithLabelValues(phase).Set(float64(h.voltageV[j]))
		mainPowerW.WithLabelValues(phase).Set(w)
		mainEnergyWhTotal.WithLabelValues(phase).Add(deltas[j])
	}
	mainCurrentA.WithLabelValues(e450Phases[3]).Set(float64(h.currentMA[0]+h.currentMA[1]+h.currentMA[2]) / 1000)
	mainPowerW.WithLabelValues(e450Phases[3]).Set(totalW)
	mainEnergyWhTotal.WithLabelValues(e450Phases[3]).Add(deltas[3])
	mainUp.Set(1)
}

// updateUp decays main_up: a push older than e450UpTimeout means the stream
// is gone.
func (e *E450) updateUp() {
	v := 0.0
	if !e.lastPush.IsZero() && time.Since(e.lastPush) <= e450UpTimeout {
		v = 1
	}
	mainUp.Set(v)
}

// openE450TTY opens dev as a raw serial port at 2400 8N1.
func openE450TTY(dev string) (*os.File, error) {
	f, err := os.OpenFile(dev, os.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	t, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	t.Iflag = unix.IGNBRK // framing errors are dropped; the stream resyncs on the next head
	t.Oflag = 0
	t.Cflag = (t.Cflag &^ unix.CBAUD) | unix.B2400 | unix.CS8 | unix.CREAD | unix.CLOCAL
	t.Lflag = 0
	if err := unix.IoctlSetTermios(int(f.Fd()), unix.TCSETS, t); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}
