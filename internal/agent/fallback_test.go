package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jwil007/roamjev/internal/linkq"
)

type stubPolicy struct {
	out PolicyOutput
	err error
	n   int
}

func (p *stubPolicy) Name() string       { return "stub" }
func (p *stubPolicy) Notify(PolicyEvent) { p.n++ }
func (p *stubPolicy) Decide(context.Context, PolicyInput) (PolicyOutput, error) {
	return p.out, p.err
}

type linkedRadio struct{ fakeRadio }

func (r *linkedRadio) Link(context.Context) (Link, error) {
	return Link{SSID: "lab", BSSID: "aa:00", WPAState: "COMPLETED", RSSI: -60, Freq: 5180}, nil
}

func lastDecision(st *Store) Decision {
	d := st.Snapshot().Decisions
	return d[len(d)-1]
}

// The fallback decides if and only if the primary errors.
func TestFallbackOnlyWhenJevDown(t *testing.T) {
	st, _ := NewStore("")
	primary := &stubPolicy{out: PolicyOutput{Chosen: "stay", Confidence: 0.9}}
	fallback := &stubPolicy{out: PolicyOutput{Chosen: "stay", Confidence: 1, Reason: "classic rule"}}
	a := New(DefaultConfig(), &linkedRadio{}, linkq.NewRing(time.Minute), primary, st)
	a.Fallback = fallback
	again := make(chan string, 1)

	a.decide(context.Background(), "interval", again)
	if d := lastDecision(st); strings.Contains(d.Policy, "fallback") {
		t.Fatalf("fallback used while primary was up: %+v", d)
	}
	primary.err = errors.New("jev request: connection refused")
	a.decide(context.Background(), "interval", again)
	d := lastDecision(st)
	if !strings.Contains(d.Policy, "fallback") || d.Reason != "classic rule" || d.Executed != "stay" {
		t.Fatalf("expected fallback decision, got %+v", d)
	}
	if d.Err == "" {
		t.Fatal("Jev's error should still be recorded")
	}
	primary.err = nil
	a.decide(context.Background(), "interval", again)
	if d := lastDecision(st); strings.Contains(d.Policy, "fallback") {
		t.Fatalf("Jev should be back in charge: %+v", d)
	}
}

func TestSafetyNet(t *testing.T) {
	st, _ := NewStore("")
	a := New(DefaultConfig(), &linkedRadio{}, linkq.NewRing(time.Minute), &stubPolicy{}, st)
	now := time.Now()
	weak := Link{BSSID: "aa:00", RSSI: -80}
	healthy := Link{BSSID: "aa:00", RSSI: -60}
	stay := Decision{Policy: "jev", Chosen: "stay", Executed: "stay"}
	if why := a.safetyNet(Decision{Policy: "classic", Chosen: "stay", Executed: "stay"}, Link{BSSID: "aa:00", RSSI: -80}, nil, now); why != "" {
		t.Fatalf("safety net must not apply to the classic policy: %q", why)
	}
	if why := a.safetyNet(stay, weak, nil, now); !strings.HasPrefix(why, "safety net") {
		t.Fatalf("weak link, nothing better known: want a full scan, got %q", why)
	}
	if why := a.safetyNet(stay, healthy, nil, now); why != "" {
		t.Fatalf("healthy link: got %q", why)
	}
	better := []Candidate{{BSSID: "bb:01", RSSIDelta: 10, SeenAgoS: 5}}
	if why := a.safetyNet(stay, weak, better, now); why != "" {
		t.Fatalf("a fresh stronger candidate is known: got %q", why)
	}
	if why := a.safetyNet(Decision{Policy: "jev", Chosen: "roam", Executed: "roam"}, weak, nil, now); why != "" {
		t.Fatalf("Jev already roaming: got %q", why)
	}
	a.lastFull = now.Add(-10 * time.Second)
	if why := a.safetyNet(stay, weak, nil, now); why != "" {
		t.Fatalf("recent full scan: got %q", why)
	}
}

func TestDropSeconds(t *testing.T) {
	for _, c := range []struct{ rssi, fell10, fell30, want int }{
		{-76, 7, 15, 20}, // slower rate 0.5 dB/s: 9 dB in 18 s
		{-56, 14, 2, 0},  // a 10 s dip that isn't a 30 s trend
		{-60, 3, 9, 0},   // 25 dB at 0.3 dB/s is more than a minute
		{-86, 9, 20, 0},  // already past the drop point
		{-84, 10, 30, 5}, // floor of 5 s
	} {
		if got := dropSeconds(c.rssi, c.fell10, c.fell30); got != c.want {
			t.Errorf("dropSeconds(%d, %d, %d) = %d, want %d", c.rssi, c.fell10, c.fell30, got, c.want)
		}
	}
	if fromWire(wire["scan_full"]) != "scan_full" || fromWire("roam") != "roam" {
		t.Error("wire names must map back")
	}
}
