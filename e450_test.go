package main

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// The fixtures are real CII push frames captured from the building meter:
// e450_push_358.bin from the 358-byte frame layout, e450_push_376.bin from
// the 376-byte one. The frame head sits at offset 0.
var (
	head358 = head{currentMA: [3]uint16{135, 62, 91}, voltageV: [3]uint16{238, 238, 238}}
	head376 = head{currentMA: [3]uint16{70, 45, 757}, voltageV: [3]uint16{235, 234, 234}}
)

func readE450Fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

func assertGauge(t *testing.T, g prometheus.Gauge, want float64) {
	t.Helper()
	if got := testutil.ToFloat64(g); got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseHead(t *testing.T) {
	cases := []struct {
		fixture string
		want    head
	}{
		{"e450_push_358.bin", head358},
		{"e450_push_376.bin", head376},
	}
	for _, c := range cases {
		buf := readE450Fixture(t, c.fixture)
		if i := findPushHead(buf); i != 0 {
			t.Fatalf("%s: head at %d, want 0", c.fixture, i)
		}
		if got := parseHead(buf); got != c.want {
			t.Fatalf("%s: got %+v, want %+v", c.fixture, got, c.want)
		}
		// The frame body must not contain a second valid head, or the
		// resync loop would report a push that never happened.
		if i := findPushHead(buf[e450HeadLen:]); i >= 0 {
			t.Fatalf("%s: second head in body at %d", c.fixture, i)
		}
	}
}

func TestValidHead(t *testing.T) {
	h := append([]byte(nil), readE450Fixture(t, "e450_push_376.bin")[:e450HeadLen]...)
	if !validHead(h) {
		t.Fatal("fixture head must be valid")
	}
	bad := append([]byte(nil), h...)
	bad[3] = 0x11 // second tag replaced
	if validHead(bad) {
		t.Error("a replaced tag must be rejected")
	}
	bad = append([]byte(nil), h...)
	bad[10], bad[11] = 0x01, 0x90 // L1 voltage 400 V
	if validHead(bad) {
		t.Error("400 V must be rejected")
	}
	bad = append([]byte(nil), h...)
	bad[10], bad[11] = 0, 0 // L1 voltage 0 V
	if validHead(bad) {
		t.Error("0 V must be rejected")
	}
	if validHead(h[:e450HeadLen-1]) {
		t.Error("a short buffer must be rejected")
	}
}

func TestProcessResync(t *testing.T) {
	// Garbage prefix: repeated inter-frame blocks and idle bytes, as seen
	// in the captures.
	buf := bytes.Repeat([]byte{0x06, 0x00, 0xb6, 0xcd, 0xe1, 0x06}, 3)
	buf = append(buf, bytes.Repeat([]byte{0}, 4)...)
	buf = append(buf, readE450Fixture(t, "e450_push_376.bin")...)

	e := &E450{}
	e.buf = buf
	e.process()
	if i := findPushHead(e.buf); i >= 0 {
		t.Fatalf("head left in buffer at %d", i)
	}
	assertGauge(t, mainCurrentA.WithLabelValues("L1"), float64(70)/1000)
	assertGauge(t, mainCurrentA.WithLabelValues("L2"), float64(45)/1000)
	assertGauge(t, mainCurrentA.WithLabelValues("L3"), float64(757)/1000)
	assertGauge(t, mainCurrentA.WithLabelValues("L123"), float64(70+45+757)/1000)
	assertGauge(t, mainVoltageV.WithLabelValues("L1"), 235)
	assertGauge(t, mainVoltageV.WithLabelValues("L3"), 234)
	assertGauge(t, mainPowerW.WithLabelValues("L123"), power(head376))
	assertGauge(t, mainUp, 1)
}

func TestProcessChunked(t *testing.T) {
	// Feed the frame in odd-sized chunks so the head crosses a read
	// boundary.
	buf := readE450Fixture(t, "e450_push_358.bin")
	e := &E450{}
	for i := 0; i < len(buf); i += 7 {
		end := i + 7
		if end > len(buf) {
			end = len(buf)
		}
		e.buf = append(e.buf, buf[i:end]...)
		e.process()
	}
	assertGauge(t, mainCurrentA.WithLabelValues("L1"), float64(135)/1000)
	assertGauge(t, mainCurrentA.WithLabelValues("L123"), float64(135+62+91)/1000)
	assertGauge(t, mainVoltageV.WithLabelValues("L1"), 238)
	assertGauge(t, mainPowerW.WithLabelValues("L123"), power(head358))
}

func TestProcessTwoFrames(t *testing.T) {
	buf := append(readE450Fixture(t, "e450_push_358.bin"), readE450Fixture(t, "e450_push_376.bin")...)
	e := &E450{}
	var got []head
	e.onPush = func(h head, _ []byte) { got = append(got, h) }
	e.buf = buf
	e.process()
	if len(got) != 2 {
		t.Fatalf("got %d pushes, want 2", len(got))
	}
	if got[0] != head358 || got[1] != head376 {
		t.Fatalf("got %+v, want [head358 head376]", got)
	}
	if i := findPushHead(e.buf); i >= 0 {
		t.Fatalf("head left in buffer at %d", i)
	}
}

func TestStreamCapture(t *testing.T) {
	// Full captures from the building meter, one per frame layout. The
	// counts include the resync over the pre-first-frame bytes and the
	// anomalous inter-frame gaps (a missed push) present in them.
	cases := []struct {
		capture string
		heads   int
	}{
		{"e450_capture_358.bin", 11},
		{"e450_capture_376.bin", 17},
	}
	for _, c := range cases {
		buf, err := os.ReadFile("testdata/" + c.capture)
		if err != nil {
			t.Fatalf("read capture: %v", err)
		}
		var got []head
		e := &E450{}
		e.onPush = func(h head, _ []byte) { got = append(got, h) }
		e.buf = buf
		e.process()
		if len(got) != c.heads {
			t.Fatalf("%s: got %d pushes, want %d", c.capture, len(got), c.heads)
		}
	}
}

func TestEnergyDeltas(t *testing.T) {
	h := head{currentMA: [3]uint16{1000, 2000, 0}, voltageV: [3]uint16{230, 240, 250}}
	// 1 A x 230 V and 2 A x 240 V over 5 s.
	d := energyDeltas(h, 5*time.Second)
	want0 := 1.0 * 230 * 5 / 3600
	want1 := 2.0 * 240 * 5 / 3600
	if d[0] != want0 {
		t.Errorf("L1 delta: got %v, want %v", d[0], want0)
	}
	if d[1] != want1 {
		t.Errorf("L2 delta: got %v, want %v", d[1], want1)
	}
	if d[2] != 0 {
		t.Errorf("L3 delta: got %v, want 0", d[2])
	}
	if d[3] != want0+want1 {
		t.Errorf("L123 delta: got %v, want %v", d[3], want0+want1)
	}
	// No time elapsed: no energy.
	if z := energyDeltas(h, 0); z != ([4]float64{}) {
		t.Errorf("zero dt: got %v, want all zeros", z)
	}
}

func TestUpdateUpDecay(t *testing.T) {
	e := &E450{}
	e.lastPush = time.Now().Add(-e450UpTimeout - time.Second)
	e.updateUp()
	assertGauge(t, mainUp, 0)
	e.lastPush = time.Now()
	e.updateUp()
	assertGauge(t, mainUp, 1)
}

// power is the estimated total power applyPush publishes for a head.
func power(h head) float64 {
	var w float64
	for j := 0; j < 3; j++ {
		w += float64(h.currentMA[j]) / 1000 * float64(h.voltageV[j])
	}
	return w
}
