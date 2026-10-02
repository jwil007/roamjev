package agent

import (
	"strings"
	"testing"
	"time"
)

func TestRailCheck(t *testing.T) {
	now := time.Now()
	link := Link{BSSID: "aa:aa:aa:aa:aa:01"}
	tgt := "aa:aa:aa:aa:aa:02"
	cases := []struct {
		name     string
		d        Decision
		lastRoam time.Time
		lastScan time.Time
		observe  bool
		exec     string
		blocked  string
	}{
		{"stay", Decision{Chosen: "stay"}, time.Time{}, time.Time{}, false, "stay", ""},
		{"roam ok", Decision{Chosen: "roam", Target: tgt, Confidence: 0.8}, time.Time{}, time.Time{}, false, "roam", ""},
		{"roam none", Decision{Chosen: "roam", Confidence: 0.9}, time.Time{}, time.Time{}, false, "stay", "none"},
		{"roam self", Decision{Chosen: "roam", Target: link.BSSID, Confidence: 0.9}, time.Time{}, time.Time{}, false, "stay", "current AP"},
		{"roam low conf", Decision{Chosen: "roam", Target: tgt, Confidence: 0.3}, time.Time{}, time.Time{}, false, "stay", "confidence"},
		{"roam cooldown", Decision{Chosen: "roam", Target: tgt, Confidence: 0.9}, now.Add(-2 * time.Second), time.Time{}, false, "stay", "roam rail"},
		{"scan ok", Decision{Chosen: "scan_full"}, time.Time{}, now.Add(-time.Minute), false, "scan_full", ""},
		{"scan too soon", Decision{Chosen: "scan_known"}, time.Time{}, now.Add(-time.Second), false, "stay", "scan rail"},
		{"observe", Decision{Chosen: "roam", Target: tgt, Confidence: 0.9}, time.Time{}, time.Time{}, true, "stay", "observe"},
	}
	for _, c := range cases {
		cfg := DefaultConfig()
		cfg.RoamMinConfidence = 0.5 // exercise the rail even though it's off by default
		cfg.Observe = c.observe
		a := &Agent{cfg: cfg, lastRoam: c.lastRoam, scanAt: c.lastScan}
		exec, blocked := a.railCheck(c.d, link, now)
		if exec != c.exec || !strings.Contains(blocked, c.blocked) ||
			(c.blocked == "" && blocked != "") {
			t.Errorf("%s: got (%q, %q), want (%q, ~%q)",
				c.name, exec, blocked, c.exec, c.blocked)
		}
	}
}

func TestBandChannel(t *testing.T) {
	for _, c := range []struct {
		f    int
		band string
		ch   int
	}{{2437, "2.4GHz", 6}, {5180, "5GHz", 36}, {5745, "5GHz", 149}, {6135, "6GHz", 37}} {
		b, ch := bandChannel(c.f)
		if b != c.band || ch != c.ch {
			t.Errorf("bandChannel(%d) = %s %d", c.f, b, ch)
		}
	}
}

// Two retriggers in a row must not block (the second used to deadlock the
// decision loop when a scan and a pre-roam check both asked to re-decide).
func TestRetriggerNeverBlocks(t *testing.T) {
	ch := make(chan string, 1)
	done := make(chan struct{})
	go func() {
		retrigger(ch, "scan_complete")
		retrigger(ch, "verify_changed")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("retrigger blocked")
	}
	if got := <-ch; got != "scan_complete" {
		t.Fatalf("queued trigger = %q", got)
	}
}
