package sim

import (
	"context"
	"testing"
	"time"

	"github.com/jwil007/roamjev/internal/linkq"
)

// The counterfactual stream for each AP should track that AP's own signal:
// near the client it's clean, near the sensitivity floor it's lossy.
func TestCounterfactual(t *testing.T) {
	w, err := NewWorld(linkq.NewRing(time.Minute), 0, "hallway", 42)
	if err != nil {
		t.Fatal(err)
	}
	w.x, w.y = 0, 0 // standing by AP 1
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
	for _, name := range ScenarioNames {
		a, err := NewWorld(linkq.NewRing(time.Minute), 0, name, 7)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := NewWorld(linkq.NewRing(time.Minute), 0, name, 7)
		for i := range 2000 {
			d := time.Duration(i) * 250 * time.Millisecond
			a.advance(a.start.Add(d), 0.25)
			b.advance(b.start.Add(d), 0.25)
			b.probe(b.start.Add(d)) // extra noise draws in b only
			b.probe(b.start.Add(d))
		}
		if a.x != b.x || a.y != b.y || a.activity != b.activity {
			t.Fatalf("%s: worlds diverged: (%v,%v,%s) vs (%v,%v,%s)", name,
				a.x, a.y, a.activity, b.x, b.y, b.activity)
		}
		for i := range a.aps {
			if a.aps[i].shadow != b.aps[i].shadow || a.aps[i].util != b.aps[i].util {
				t.Fatalf("%s: AP %d differs", name, i)
			}
		}
	}
}

// Sanity-check each scenario's character: AP count, load, and that the
// client actually moves (except boundary).
func TestScenarios(t *testing.T) {
	for _, name := range ScenarioNames {
		w, err := NewWorld(linkq.NewRing(time.Minute), 0, name, 3)
		if err != nil {
			t.Fatal(err)
		}
		x0, y0 := w.x, w.y
		var utilSum float64
		moved := false
		for i := range 4 * 600 { // 10 simulated minutes
			w.advance(w.start.Add(time.Duration(i)*250*time.Millisecond), 0.25)
			moved = moved || w.x != x0 || w.y != y0
		}
		for _, a := range w.aps {
			utilSum += a.util
		}
		t.Logf("%-10s radios=%2d avg util=%4.0f%% moved=%v pos=(%.0f,%.0f)",
			name, len(w.aps), utilSum/float64(len(w.aps)), moved, w.x, w.y)
		if name == "boundary" == moved {
			t.Errorf("%s: moved=%v", name, moved)
		}
	}
}

func TestScanCost(t *testing.T) {
	var full float64
	for _, f := range fullScanFreqs() {
		full += dwellMs(f)
	}
	if n := len(fullScanFreqs()); n != 51 {
		t.Fatalf("full scan has %d channels", n)
	}
	if full < 2500 || full > 3500 {
		t.Fatalf("full scan dwell %.0f ms", full)
	}
}

func TestNeighborReport(t *testing.T) {
	office, _ := NewWorld(linkq.NewRing(time.Minute), 0, "office", 1)
	full, err := office.NeighborFreqs(context.Background())
	if err != nil || len(full) == 0 {
		t.Fatalf("office neighbors = %v, %v", full, err)
	}
	hall, _ := NewWorld(linkq.NewRing(time.Minute), 0, "hallway", 1)
	if f, err := hall.NeighborFreqs(context.Background()); err == nil {
		t.Fatalf("hallway should not support 802.11k, got %v", f)
	}
	// Partial lists are stable across requests.
	hosp, _ := NewWorld(linkq.NewRing(time.Minute), 0, "hospital", 1)
	a, _ := hosp.NeighborFreqs(context.Background())
	b, _ := hosp.NeighborFreqs(context.Background())
	if len(a) != len(b) {
		t.Fatalf("partial neighbor list changed between requests: %v vs %v", a, b)
	}
}

// In the corridor, the opening full scan can't hear the far end: only a
// later full scan, after walking, finds those APs.
func TestCorridorNeedsRescan(t *testing.T) {
	w, err := NewWorld(linkq.NewRing(time.Minute), 0, "corridor", 5)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Scan(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	res, _ := w.ScanResults(context.Background(), "")
	maxX := 0.0
	for _, b := range res {
		for _, a := range w.aps {
			if a.bssid == b.BSSID && a.x > maxX {
				maxX = a.x
			}
		}
	}
	if maxX >= 200 {
		t.Fatalf("opening scan from x=0 heard an AP at x=%.0f; the far end should be out of range", maxX)
	}
	if _, err := w.NeighborFreqs(context.Background()); err == nil {
		t.Fatal("corridor APs should not provide 802.11k neighbor reports")
	}
}
