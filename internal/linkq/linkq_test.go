package linkq

import (
	"math"
	"testing"
	"time"
)

func TestMOS(t *testing.T) {
	cases := []struct {
		lat, jit, loss float64
		lo, hi         float64
	}{
		{2, 0.5, 0, 4.39, 4.42},  // clean single hop: near the 4.41 ceiling
		{2, 0.5, 5, 3.9, 4.15},   // 5% loss costs ~0.3 MOS
		{2, 0.5, 20, 2.15, 2.25}, // heavy loss: R = 42.9
		{300, 50, 0, 2.5, 3.5},   // long effective latency
		{2, 0.5, 100, 1.0, 1.01}, // nothing gets through
	}
	for _, c := range cases {
		m, _ := MOS(c.lat, c.jit, c.loss)
		if m < c.lo || m > c.hi {
			t.Errorf("MOS(%v,%v,%v) = %.3f, want %.2f..%.2f",
				c.lat, c.jit, c.loss, m, c.lo, c.hi)
		}
	}
}

func TestWindow(t *testing.T) {
	r := NewRing(time.Minute)
	t0 := time.Now().Add(-10 * time.Second)
	rtts := []float64{2, 4, 2, 4}
	for i, ms := range rtts {
		r.Add(Sample{At: t0.Add(time.Duration(i) * time.Second),
			RTT: time.Duration(ms * float64(time.Millisecond))})
	}
	r.Add(Sample{At: t0.Add(4 * time.Second), Lost: true})
	w := r.Between(t0, t0.Add(5*time.Second))
	if w.Sent != 5 || w.Lost != 1 || w.LossPct != 20 {
		t.Fatalf("sent/lost/loss = %d/%d/%v", w.Sent, w.Lost, w.LossPct)
	}
	if math.Abs(w.LatencyMs-3) > 1e-9 || math.Abs(w.JitterMs-2) > 1e-9 {
		t.Fatalf("latency/jitter = %v/%v, want 3/2", w.LatencyMs, w.JitterMs)
	}
	// The end bound is exclusive.
	if w := r.Between(t0, t0.Add(time.Second)); w.Sent != 1 {
		t.Fatalf("exclusive end: sent = %d", w.Sent)
	}
}

func TestReadTCPCounters(t *testing.T) {
	c, err := ReadTCPCounters()
	if err != nil {
		t.Fatal(err)
	}
	if c.OutSegs == 0 {
		t.Fatal("OutSegs is zero")
	}
}
