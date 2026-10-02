package agent

import (
	"context"
	"fmt"
	"hash/fnv"
	"slices"
	"time"

	"github.com/jwil007/roamjev/internal/config"
	"github.com/jwil007/roamjev/internal/roam"
	"github.com/jwil007/roamjev/internal/wpac"
)

// ClassicPolicy is roamctl's algorithm, ported to the Policy interface so it
// can be compared against Jev on identical measurement, cadence, rails and
// grading. Scoring calls roamctl's own score function (roam.ScoreBSS) with
// roamctl's default configuration. The control flow mirrors roam.Proc and
// the tier handlers in internal/roam/roam.go:
//
//   - tiers from RSSI (excellent/fair/degraded/critical) with tier
//     hysteresis; an unhealthy link (low MCS or bitrate) forces critical
//   - excellent: no scanning or roaming
//   - fair: background scan every base_scan_interval, roam if the best AP
//     beats the current one by fair_score_delta
//   - degraded: scan on entry, then as fair with degraded_score_delta
//   - critical: scan on entry; if no candidate, a break-glass full scan;
//     critical_score_delta
//   - after a roam, RSSI hysteresis suppresses roam logic until RSSI moves
//     rssi_hysteresis_up/down from where the roam was triggered
//   - failed roams penalize the AP's score for 1 min (60 min if repeated)
//   - connection_cooldown after any connection change
//
// Deviations, all forced by sharing the agent's loop: decisions happen at the
// agent's cadence (3 s plus after each scan) instead of every 100 ms, RSSI
// is the driver's beacon average instead of a 5-sample smoothing window, and
// the retry-rate health check is skipped (drivers don't report it reliably).
type ClassicPolicy struct {
	cfg *config.Config

	tier                                       classicTier
	entryScanned, entryScannedCrit, fullScanCr bool
	hysteresis                                 bool
	triggerRSSI                                int
	lastScan                                   time.Time
	evaluatedScan                              time.Time
	fullScanNext                               bool
	bssHash                                    uint64
	lastConnChange                             time.Time
	pendingRoamRSSI                            int
	penalties                                  map[string]*classicPenalty
}

type classicTier int

const (
	tierExcellent classicTier = iota
	tierFair
	tierDegraded
	tierCritical
)

func (t classicTier) String() string {
	return [...]string{"excellent", "fair", "degraded", "critical"}[t]
}

type classicPenalty struct {
	fails    int
	lastFail time.Time
}

func NewClassicPolicy(cfg *config.Config) *ClassicPolicy {
	return &ClassicPolicy{cfg: cfg, penalties: map[string]*classicPenalty{}}
}

func (p *ClassicPolicy) Name() string { return "classic" }

func (p *ClassicPolicy) Notify(ev PolicyEvent) {
	switch ev.Kind {
	case "scan":
		if ev.Success {
			p.lastScan = ev.At
			if ev.Message == "full" {
				p.fullScanNext = false
			}
		}
	case "conn_change":
		p.lastConnChange = ev.At
	case "roam":
		if ev.Success {
			p.lastConnChange = ev.At
			delete(p.penalties, ev.Target)
			p.hysteresis = true
			p.triggerRSSI = p.pendingRoamRSSI
			return
		}
		pen := p.penalties[ev.Target]
		if pen == nil {
			pen = &classicPenalty{}
			p.penalties[ev.Target] = pen
		}
		pen.fails++
		pen.lastFail = ev.At
	}
}

func stay(reason string, args ...any) PolicyOutput {
	return PolicyOutput{Chosen: "stay", Confidence: 1,
		Reason: fmt.Sprintf(reason, args...)}
}

func (p *ClassicPolicy) Decide(_ context.Context, in PolicyInput) (PolicyOutput, error) {
	cfg := p.cfg
	rssi := in.Link.RSSI
	if !p.lastConnChange.IsZero() &&
		in.Now.Sub(p.lastConnChange) <= cfg.ConnectionCooldown {
		return stay("connection cooldown (%s)", cfg.ConnectionCooldown), nil
	}
	if p.hysteresis {
		up := p.triggerRSSI + cfg.RSSIHysteresisUp
		down := p.triggerRSSI - cfg.RSSIHysteresisDown
		if rssi >= up || rssi <= down {
			p.hysteresis = false
		} else {
			return stay("RSSI hysteresis: %d dBm must leave %d..%d first",
				rssi, down, up), nil
		}
	}
	unhealthy := p.unhealthy(in.Link)
	p.evalTier(rssi, unhealthy)
	if rssi >= cfg.FairRSSI+cfg.TierHysteresis &&
		(p.entryScanned || p.entryScannedCrit) && !unhealthy {
		p.entryScanned, p.entryScannedCrit, p.fullScanCr = false, false, false
	}
	tierNote := fmt.Sprintf("%s tier (%d dBm)", p.tier, rssi)
	if unhealthy {
		tierNote = "critical tier (unhealthy link: low MCS/bitrate)"
	}

	switch p.tier {
	case tierExcellent:
		return stay("%s: no scanning or roaming", tierNote), nil
	case tierDegraded:
		if !p.entryScanned {
			p.entryScanned = true
			return p.scan(false, "%s: scan on entry", tierNote), nil
		}
	case tierCritical:
		if !p.entryScannedCrit {
			p.entryScannedCrit = true
			return p.scan(false, "%s: scan on entry", tierNote), nil
		}
	}

	// Evaluate only when there's a scan it hasn't evaluated yet, as roamctl
	// does with checkIfNewScan.
	if !in.ScanAt.IsZero() && in.ScanAt.After(p.evaluatedScan) {
		p.evaluatedScan = in.ScanAt
		out, noCandidates := p.evaluate(in, unhealthy, tierNote)
		if out.Chosen == "roam" {
			p.pendingRoamRSSI = rssi
			return out, nil
		}
		if p.tier == tierCritical && noCandidates && !p.fullScanCr {
			p.fullScanCr = true
			return p.scan(true, "%s: no better AP, break-glass full scan",
				tierNote), nil
		}
		return out, nil
	}
	if p.lastScan.IsZero() || in.Now.Sub(p.lastScan) >= cfg.BGScanInterval {
		return p.scan(p.fullScanNext, "%s: background scan (every %s)",
			tierNote, cfg.BGScanInterval), nil
	}
	return stay("%s: waiting for next background scan", tierNote), nil
}

func (p *ClassicPolicy) scan(full bool, reason string, args ...any) PolicyOutput {
	o := PolicyOutput{Chosen: "scan_targeted", Confidence: 1,
		Reason: fmt.Sprintf(reason, args...)}
	if full {
		o.Chosen = "scan_full"
	}
	return o
}

func (p *ClassicPolicy) evalTier(rssi int, unhealthy bool) {
	if unhealthy {
		p.tier = tierCritical
		return
	}
	buf := func(t classicTier) int {
		if p.tier > t {
			return p.cfg.TierHysteresis
		}
		return 0
	}
	switch {
	case rssi >= p.cfg.ExcellentRSSI+buf(tierExcellent):
		p.tier = tierExcellent
	case rssi >= p.cfg.FairRSSI+buf(tierFair):
		p.tier = tierFair
	case rssi >= p.cfg.DegradedRSSI+buf(tierDegraded):
		p.tier = tierDegraded
	default:
		p.tier = tierCritical
	}
}

// unhealthy mirrors roam.checkConnectionHealth minus the retry rate.
func (p *ClassicPolicy) unhealthy(l Link) bool {
	if l.TxBitrate < 1_000_000 || l.RxBitrate < 1_000_000 {
		return false
	}
	legacy := []int{1000000, 2000000, 5500000, 6000000, 9000000, 11000000,
		12000000, 18000000, 24000000, 36000000, 48000000, 54000000}
	best := max(l.TxBitrate, l.RxBitrate)
	if slices.Contains(legacy, best) {
		return false
	}
	return best <= p.cfg.DataRate*1_000_000 ||
		max(l.TxMCS, l.RxMCS) <= p.cfg.MCSIndex
}

func (p *ClassicPolicy) evaluate(in PolicyInput, unhealthy bool,
	tierNote string) (PolicyOutput, bool) {
	type scored struct {
		bssid string
		score int
	}
	var list []scored
	cur := scored{bssid: in.Link.BSSID}
	for _, b := range in.Scan {
		s := roam.ScoreBSS(toRich(b), p.cfg)
		if pen := p.penalties[b.BSSID]; pen != nil {
			timer := time.Minute
			if pen.fails > 1 {
				timer = time.Hour
			}
			if in.Now.Sub(pen.lastFail) > timer {
				delete(p.penalties, b.BSSID)
			} else {
				s -= pen.fails * p.cfg.UnhealthyScoreMod
			}
		}
		if b.BSSID == in.Link.BSSID {
			if unhealthy {
				s = max(0, s-p.cfg.UnhealthyScoreMod)
			}
			cur.score = s
		}
		list = append(list, scored{b.BSSID, s})
	}
	if len(list) == 0 {
		return stay("%s: scan found no APs", tierNote), true
	}
	slices.SortFunc(list, func(a, b scored) int { return b.score - a.score })
	best := list[0]

	// A changed BSS list means the next background scan should be full.
	h := fnv.New64a()
	ids := make([]string, 0, len(in.Scan))
	for i, b := range in.Scan {
		if i >= p.cfg.MaxBSSCt {
			break
		}
		ids = append(ids, b.BSSID)
	}
	slices.Sort(ids)
	for _, id := range ids {
		_, _ = h.Write([]byte(id))
	}
	sum := h.Sum64()
	if p.bssHash != 0 && sum != p.bssHash {
		p.fullScanNext = true
	}
	p.bssHash = sum

	if best.bssid == cur.bssid {
		return stay("%s: current AP scores best (%d)", tierNote, cur.score), true
	}
	delta := map[classicTier]int{tierFair: p.cfg.FairDelta,
		tierDegraded: p.cfg.DegradedDelta, tierCritical: p.cfg.CriticalDelta}[p.tier]
	if best.score-delta >= cur.score {
		return PolicyOutput{Chosen: "roam", Target: best.bssid, Confidence: 1,
			Reason: fmt.Sprintf("%s: best AP scores %d vs current %d "+
				"(needs +%d)", tierNote, best.score, cur.score, delta)}, false
	}
	return stay("%s: best AP scores %d vs current %d, below the +%d delta",
		tierNote, best.score, cur.score, delta), true
}

// toRich converts a scan result back into roamctl's type for scoring.
func toRich(b BSS) wpac.RichBSS {
	r := wpac.RichBSS{}
	r.BSSID, r.Freq, r.RSSI, r.SNR, r.Age = b.BSSID, b.Freq, b.RSSI, b.SNR, b.Age
	r.ChannelNum = b.Channel
	r.Band = map[string]wpac.Band{"2.4GHz": wpac.Band2point4,
		"5GHz": wpac.Band5, "6GHz": wpac.Band6}[b.Band]
	r.ChannelWidth = map[string]wpac.ChannelWidth{"20MHz": wpac.ChannelWidth20,
		"40MHz": wpac.ChannelWidth40, "80MHz": wpac.ChannelWidth80,
		"160MHz": wpac.ChannelWidth160, "80+80MHz": wpac.ChannelWidth80Plus80,
		"320MHz": wpac.ChannelWidth320}[b.Width]
	r.PHYType = map[string]wpac.PHYType{"Legacy a/b/g": wpac.PHYLegacy,
		"802.11n": wpac.PHY80211n, "802.11ac": wpac.PHY80211ac,
		"802.11ax": wpac.PHY80211ax, "802.11be": wpac.PHY80211be}[b.PHY]
	if b.UtilPct >= 0 {
		r.QBSSUtil = uint8(b.UtilPct * 255 / 100)
	}
	return r
}
