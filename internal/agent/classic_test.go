package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jwil007/roamjev/internal/config"
)

func classic(t *testing.T) *ClassicPolicy {
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	return NewClassicPolicy(cfg)
}

func bss(id string, rssi int) BSS {
	return BSS{BSSID: id, RSSI: rssi, SNR: rssi + 95, Band: "5GHz",
		Width: "80MHz", PHY: "802.11ax", UtilPct: 20}
}

func link(id string, rssi int) Link {
	return Link{BSSID: id, RSSI: rssi, TxBitrate: 400e6, RxBitrate: 400e6,
		TxMCS: 9, RxMCS: 9}
}

func TestClassicTiers(t *testing.T) {
	p := classic(t)
	now := time.Now()
	ctx := context.Background()
	in := func(l Link, scan []BSS, scanAt time.Time) PolicyInput {
		return PolicyInput{Now: now, Link: l, Scan: scan, ScanAt: scanAt}
	}
	// Excellent (>= -50): nothing.
	out, _ := p.Decide(ctx, in(link(bA, -45), nil, time.Time{}))
	if out.Chosen != "stay" || !strings.Contains(out.Reason, "excellent") {
		t.Fatalf("excellent: %+v", out)
	}
	// Degraded (-65 > rssi >= -73): scan on entry.
	out, _ = p.Decide(ctx, in(link(bA, -70), nil, time.Time{}))
	if out.Chosen != "scan_targeted" || !strings.Contains(out.Reason, "entry") {
		t.Fatalf("degraded entry: %+v", out)
	}
	// New scan: B is much stronger, beats the degraded delta (6) -> roam.
	scanAt := now
	scan := []BSS{bss(bA, -70), bss(bB, -50)}
	out, _ = p.Decide(ctx, in(link(bA, -70), scan, scanAt))
	if out.Chosen != "roam" || out.Target != bB {
		t.Fatalf("degraded roam: %+v", out)
	}
	// After a successful roam: cooldown, then RSSI hysteresis.
	p.Notify(PolicyEvent{Kind: "roam", At: now, Target: bB, Success: true})
	out, _ = p.Decide(ctx, in(link(bB, -72), scan, scanAt))
	if !strings.Contains(out.Reason, "cooldown") {
		t.Fatalf("cooldown: %+v", out)
	}
	now = now.Add(10 * time.Second)
	out, _ = p.Decide(ctx, in(link(bB, -72), scan, scanAt))
	if !strings.Contains(out.Reason, "hysteresis") {
		t.Fatalf("hysteresis: %+v", out)
	}
}

func TestClassicDeltaAndPenalty(t *testing.T) {
	p := classic(t)
	now := time.Now()
	ctx := context.Background()
	p.tier, p.entryScanned = tierFair, true
	// Fair tier, B only slightly better than A: below the fair delta (7).
	scan := []BSS{bss(bA, -62), bss(bB, -60)}
	out, _ := p.Decide(ctx, PolicyInput{Now: now, Link: link(bA, -62), Scan: scan, ScanAt: now})
	if out.Chosen != "stay" || !strings.Contains(out.Reason, "below") {
		t.Fatalf("small delta: %+v", out)
	}
	// A failed roam to C penalizes it: C at -48 would win, but not with -20.
	p.Notify(PolicyEvent{Kind: "roam", At: now, Target: bC, Success: false})
	scan = []BSS{bss(bA, -62), bss(bC, -48)}
	out, _ = p.Decide(ctx, PolicyInput{Now: now.Add(time.Second), Link: link(bA, -62), Scan: scan, ScanAt: now.Add(time.Second)})
	if out.Chosen == "roam" {
		t.Fatalf("penalized AP chosen: %+v", out)
	}
	// After the 1-minute penalty timer it's eligible again.
	later := now.Add(2 * time.Minute)
	out, _ = p.Decide(ctx, PolicyInput{Now: later, Link: link(bA, -62), Scan: scan, ScanAt: later})
	if out.Chosen != "roam" || out.Target != bC {
		t.Fatalf("penalty expired: %+v", out)
	}
}

func TestClassicCriticalBreakGlass(t *testing.T) {
	p := classic(t)
	now := time.Now()
	ctx := context.Background()
	out, _ := p.Decide(ctx, PolicyInput{Now: now, Link: link(bA, -80)})
	if out.Chosen != "scan_targeted" {
		t.Fatalf("critical entry: %+v", out)
	}
	// Scan shows nothing better: full scan next.
	scan := []BSS{bss(bA, -80), bss(bB, -84)}
	out, _ = p.Decide(ctx, PolicyInput{Now: now, Link: link(bA, -80), Scan: scan, ScanAt: now})
	if out.Chosen != "scan_full" {
		t.Fatalf("break-glass: %+v", out)
	}
}
