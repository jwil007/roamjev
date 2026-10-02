package sim

import (
	"testing"
	"time"

	"github.com/jwil007/roamjev/internal/linkq"
)

// The counterfactual stream for each AP should track that AP's own signal:
// near the client it's clean, near the sensitivity floor it's lossy.
func TestCounterfactual(t *testing.T) {
	w, err := NewWorld(linkq.NewRing(time.Minute), 0, "walk")
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
