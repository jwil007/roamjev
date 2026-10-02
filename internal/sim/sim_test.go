package sim

import (
	"testing"
	"time"

	"github.com/jwil007/roamjev/internal/linkq"
)

// The counterfactual stream for each AP should track that AP's own signal:
// near the client it's clean, near the sensitivity floor it's lossy.
func TestCounterfactual(t *testing.T) {
	w, err := NewWorld(linkq.NewRing(time.Minute), 0, "walk", 42)
	if err != nil {
		t.Fatal(err)
	}
	w.x = 0 // standing under AP 1
	start := time.Now()
	for i := range 200 {
		w.probeCounterfactuals(start.Add(time.Duration(i) * 250 * time.Millisecond))
	}
	end := start.Add(time.Minute)
	near, ok := w.StayQuality(w.aps[0].bssid, start, end)
	if !ok || near.MOS < 4.2 {
		t.Fatalf("near AP: ok=%v MOS=%.2f", ok, near.MOS)
	}
	// 6 GHz radio 78 m away sits near the -88 dBm floor: heavy loss.
	far, ok := w.StayQuality(w.aps[7].bssid, start, end)
	if !ok || far.LossPct < 20 || far.MOS > near.MOS-1 {
		t.Fatalf("far AP: ok=%v loss=%.0f%% MOS=%.2f", ok, far.LossPct, far.MOS)
	}
	if _, ok := w.StayQuality("no:such", start, end); ok {
		t.Fatal("unknown BSSID reported valid")
	}
}

// Two worlds with the same seed evolve identically even when one of them
// draws extra measurement noise, because the world has its own stream.
func TestSeedDeterminism(t *testing.T) {
	a, _ := NewWorld(linkq.NewRing(time.Minute), 1.2, "walk", 7)
	b, _ := NewWorld(linkq.NewRing(time.Minute), 1.2, "walk", 7)
	now := time.Now()
	for i := range 400 {
		at := now.Add(time.Duration(i) * 250 * time.Millisecond)
		a.advance(at, 0.25)
		b.advance(at, 0.25)
		b.probe(at) // extra noise draws in b only
		b.probe(at)
	}
	if a.x != b.x {
		t.Fatalf("positions differ: %v vs %v", a.x, b.x)
	}
	for i := range a.aps {
		if a.aps[i].shadow != b.aps[i].shadow || a.aps[i].util != b.aps[i].util {
			t.Fatalf("AP %d differs", i)
		}
	}
}
