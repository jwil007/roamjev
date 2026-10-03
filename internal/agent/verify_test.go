package agent

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jwil007/roamjev/internal/linkq"
)

// fakeRadio returns a fixed scan result for verification tests.
type fakeRadio struct {
	res   []BSS
	scans [][]int
}

func (f *fakeRadio) Link(context.Context) (Link, error) { return Link{}, nil }
func (f *fakeRadio) Scan(_ context.Context, freqs []int) error {
	f.scans = append(f.scans, freqs)
	return nil
}
func (f *fakeRadio) ScanResults(context.Context, string) ([]BSS, error) { return f.res, nil }
func (f *fakeRadio) Roam(context.Context, string) (RoamResult, error)   { return RoamResult{}, nil }
func (f *fakeRadio) Prepare() (func(), error)                           { return func() {}, nil }
func (f *fakeRadio) Counters() (uint64, uint64, error)                  { return 0, 0, nil }

func TestVerifyTarget(t *testing.T) {
	cases := []struct {
		name  string
		fresh []BSS
		want  string
	}{
		{"still there", []BSS{{BSSID: bB, Freq: 5745, RSSI: -52}}, ""},
		{"faded", []BSS{{BSSID: bB, Freq: 5745, RSSI: -62}}, "verify_changed"},
		{"stronger", []BSS{{BSSID: bB, Freq: 5745, RSSI: -40}}, "verify_changed"},
		{"gone", []BSS{{BSSID: bA, Freq: 5180, RSSI: -60}}, "verify_gone"},
		{"stale entry", []BSS{{BSSID: bB, Freq: 5745, RSSI: -50, Age: 30 * time.Second}}, "verify_gone"},
	}
	for _, c := range cases {
		r := &fakeRadio{res: c.fresh}
		st, _ := NewStore("")
		a := New(DefaultConfig(), r, linkq.NewRing(time.Minute), NewClassicPolicy(nil), st)
		a.cfg.VerifyRoam = true
		a.scan = []BSS{{BSSID: bB, Freq: 5745, RSSI: -50}}
		got := a.verifyTarget(context.Background(), Decision{Target: bB}, Link{BSSID: bA})
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
		if len(r.scans) != 1 || len(r.scans[0]) != 1 || r.scans[0][0] != 5745 {
			t.Errorf("%s: scanned %v, want just the target channel", c.name, r.scans)
		}
	}
}

// Repeated quick scans must rotate through every known channel, not keep
// re-measuring the strongest ones.
func TestQuickScanRotates(t *testing.T) {
	st, _ := NewStore("")
	a := New(DefaultConfig(), &fakeRadio{}, linkq.NewRing(time.Minute), NewClassicPolicy(nil), st)
	link := Link{BSSID: "aa:00", Freq: 5180}
	a.scan = []BSS{
		{BSSID: "aa:00", Freq: 5180, RSSI: -50, Band: "5GHz"},
		{BSSID: "bb:01", Freq: 5745, RSSI: -55, Band: "5GHz"}, // strongest candidate
		{BSSID: "cc:02", Freq: 6135, RSSI: -80, Band: "6GHz"},
		{BSSID: "dd:03", Freq: 5260, RSSI: -84, Band: "5GHz"},
		{BSSID: "ee:04", Freq: 2437, RSSI: -70, Band: "2.4GHz"},
	}
	cands := a.buildCandidates(link, time.Now())
	seen := map[int]bool{}
	now := time.Now()
	for i := range 4 {
		quick, _ := a.scanScopes(link, cands)
		if len(quick) != 2 || !slices.Contains(quick, 5745) {
			t.Fatalf("scan %d: quick = %v, want strongest (5745) + one stale channel", i, quick)
		}
		for _, f := range quick {
			if f != 5745 {
				seen[f] = true
			}
		}
		a.markScanned(quick, nil, now.Add(time.Duration(i)*time.Second))
	}
	for _, f := range []int{6135, 5260, 2437} {
		if !seen[f] {
			t.Errorf("channel %d never refreshed by quick scans (saw %v)", f, seen)
		}
	}
}
