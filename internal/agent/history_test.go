package agent

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	bA = "aa:aa:aa:00:00:0a"
	bB = "aa:aa:aa:00:00:0b"
	bC = "aa:aa:aa:00:00:0c"
)

func agentWithJoins(start time.Time, seq []string, gap time.Duration) *Agent {
	a := &Agent{}
	for i, b := range seq {
		a.recordJoin(b, start.Add(time.Duration(i)*gap))
	}
	return a
}

func TestHistoryOf(t *testing.T) {
	start := time.Now().Add(-2 * time.Minute)
	a := agentWithJoins(start, []string{bA, bB, bA, bB}, 20*time.Second)
	now := start.Add(70 * time.Second)
	hA := a.historyOf(bA, now)
	// A: first association (not a roam) + one roam back = 1 join.
	if hA.joins != 1 || hA.stays != 2 || hA.avgStay != 20*time.Second {
		t.Fatalf("A history = %+v", hA)
	}
	if !hA.lastLeft.Equal(start.Add(60 * time.Second)) {
		t.Fatalf("A lastLeft = %v", hA.lastLeft)
	}
	hB := a.historyOf(bB, now)
	if hB.joins != 2 || hB.stays != 1 {
		t.Fatalf("B history = %+v", hB)
	}
	if got := a.roamsWithin(now, historyWindow); got != 3 {
		t.Fatalf("roamsWithin = %d", got)
	}
	w := hB.words(now, true)
	if !strings.Contains(w, "2 times") || strings.Contains(w, "left") {
		t.Fatalf("current-AP words = %q", w)
	}
}

func TestPingPong(t *testing.T) {
	start := time.Now().Add(-90 * time.Second)
	now := start.Add(80 * time.Second)
	a := agentWithJoins(start, []string{bA, bB, bA, bB}, 20*time.Second)
	p := a.pingPong(now)
	if !strings.Contains(p, "3 times") || !strings.Contains(p, apID(bA)) {
		t.Fatalf("pingPong = %q", p)
	}
	// A -> B -> C is movement, not ping-pong.
	if p := agentWithJoins(start, []string{bA, bB, bC}, 20*time.Second).pingPong(now); p != "" {
		t.Fatalf("A,B,C flagged: %q", p)
	}
	// A single roam is not a pattern.
	if p := agentWithJoins(start, []string{bA, bB}, 20*time.Second).pingPong(now); p != "" {
		t.Fatalf("A,B flagged: %q", p)
	}
	// The pattern is the alternating tail: C, A, B, A counts 2 switches.
	p = agentWithJoins(start, []string{bC, bA, bB, bA}, 20*time.Second).pingPong(now)
	if !strings.Contains(p, "2 times") {
		t.Fatalf("tail pingPong = %q", p)
	}
}

func TestPingPongMobility(t *testing.T) {
	start := time.Now().Add(-90 * time.Second)
	now := start.Add(80 * time.Second)
	build := func(first, last int) *Agent {
		a := &Agent{}
		for i, b := range []string{bA, bB, bA, bB} {
			a.recordJoin(b, start.Add(time.Duration(i)*20*time.Second))
			a.observeRSSI(b, first)
			a.observeRSSI(b, last)
		}
		return a
	}
	if p := build(-76, -78).pingPong(now); !strings.Contains(p, "no sign the client is moving") {
		t.Fatalf("stationary: %q", p)
	}
	if p := build(-50, -72).pingPong(now); !strings.Contains(p, "fallen 22-22 dB") {
		t.Fatalf("walking: %q", p)
	}
	// A big rise on the AP being left is neither case.
	if p := build(-80, -66).pingPong(now); strings.Contains(p, "no sign") || strings.Contains(p, "consistent") {
		t.Fatalf("rise: %q", p)
	}
}

func TestStayFall(t *testing.T) {
	if f := stayFall([]int{-50, -51, -49, -60, -70, -72, -71}); f != 21 {
		t.Fatalf("stayFall = %d, want 21", f)
	}
	if f := stayFall([]int{-60}); f != 0 {
		t.Fatalf("single reading: %d", f)
	}
}

func TestSelectCandidatesBandBalance(t *testing.T) {
	var in []Candidate
	for i := range 6 { // six strong 2.4 GHz radios
		in = append(in, Candidate{BSSID: fmt.Sprintf("aa:%02d", i), Band: "2.4GHz", RSSI: -50 - i})
	}
	in = append(in, Candidate{BSSID: "bb:01", Band: "5GHz", RSSI: -66},
		Candidate{BSSID: "bb:02", Band: "5GHz", RSSI: -70},
		Candidate{BSSID: "cc:01", Band: "6GHz", RSSI: -72})
	got := selectCandidates(in, 6)
	bands := map[string]int{}
	for _, c := range got {
		bands[c.Band]++
	}
	if bands["5GHz"] != 2 || bands["6GHz"] != 1 || len(got) != 6 {
		t.Fatalf("bands = %v (n=%d)", bands, len(got))
	}
}
