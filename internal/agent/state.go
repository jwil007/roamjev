package agent

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/jwil007/roamjev/internal/jev"
	"github.com/jwil007/roamjev/internal/linkq"
)

// This file turns measurements into what Jev reads. TypeSafe's guidance
// (https://docs.typesafe.ai/concepts/state.md, .../model-jaggedness/jev-1.13.md)
// is that Jev reads literally, is weak at arithmetic and numeric comparison,
// and loses accuracy with irrelevant context. So code does all arithmetic
// (deltas, trends, ages) and attaches plain-language readings next to the
// raw numbers, but never decides anything: there are no thresholds that
// trigger a roam or a scan here.

// Candidate is a roam target as presented to Jev (and to the UI).
type Candidate struct {
	ID        string  `json:"id"`
	BSSID     string  `json:"bssid"`
	Band      string  `json:"band"`
	Channel   int     `json:"channel"`
	Freq      int     `json:"freq"`
	Width     string  `json:"width"`
	PHY       string  `json:"phy"`
	RSSI      int     `json:"rssi_dbm"`
	RSSIDelta int     `json:"rssi_vs_current_db"`
	SNR       int     `json:"snr_db"`
	UtilPct   int     `json:"channel_utilization_pct"`
	Stations  int     `json:"stations"`
	EstMbps   int     `json:"est_throughput_mbps"`
	SeenAgoS  int     `json:"measured_seconds_ago"`
	History   string  `json:"history,omitempty"`
	Current   bool    `json:"current,omitempty"`
	Prob      float64 `json:"prob,omitempty"`
}

// bssMemory is what we remember about an AP across decisions, so Jev can see
// the consequences of its earlier choices.
type bssMemory struct {
	failures    int
	lastFailAt  time.Time
	lastFailMsg string
	lastOutcome *Outcome
}

func apID(bssid string) string {
	h := strings.ReplaceAll(bssid, ":", "")
	if len(h) >= 6 {
		h = h[len(h)-6:]
	}
	return "ap_" + h
}

func mosRating(m float64) string {
	// ITU-T G.107 user-satisfaction bands (R 90/80/70/60/50).
	switch {
	case m >= 4.34:
		return "excellent"
	case m >= 4.03:
		return "good"
	case m >= 3.6:
		return "fair, some users dissatisfied"
	case m >= 3.1:
		return "poor, many users dissatisfied"
	default:
		return "bad, nearly all users dissatisfied"
	}
}

func ago(d time.Duration) string {
	switch {
	case d < 90*time.Second:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < 90*time.Minute:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%.1f h ago", d.Hours())
	}
}

func trendWords(delta float64, unit string, over string, steady float64) string {
	switch {
	case math.Abs(delta) < steady:
		return "steady over " + over
	case delta > 0:
		return fmt.Sprintf("rising %.1f %s over %s", delta, unit, over)
	default:
		return fmt.Sprintf("falling %.1f %s over %s", -delta, unit, over)
	}
}

func (a *Agent) memoryLine(bssid string, now time.Time) string {
	m := a.mem[bssid]
	if m == nil {
		return ""
	}
	var parts []string
	if m.lastOutcome != nil {
		o := m.lastOutcome
		parts = append(parts, fmt.Sprintf(
			"last roam here %s: MOS %.2f -> %.2f (%s)",
			ago(now.Sub(o.T)), o.Pre.MOS, o.Post.MOS, o.Verdict))
	}
	if m.failures > 0 {
		parts = append(parts, fmt.Sprintf(
			"%d failed roam attempt(s), latest %s: %s",
			m.failures, ago(now.Sub(m.lastFailAt)), m.lastFailMsg))
	}
	return strings.Join(parts, "; ")
}

func (a *Agent) buildCandidates(link Link, now time.Time) []Candidate {
	var out []Candidate
	scanAge := now.Sub(a.scanAt)
	for _, b := range a.scan {
		c := Candidate{
			ID: apID(b.BSSID), BSSID: b.BSSID, Band: b.Band,
			Channel: b.Channel, Freq: b.Freq, Width: b.Width, PHY: b.PHY,
			RSSI: b.RSSI, RSSIDelta: b.RSSI - link.RSSI, SNR: b.SNR,
			UtilPct: b.UtilPct, Stations: b.Stations,
			EstMbps:  b.EstThroughputKbps / 1000,
			SeenAgoS: int((scanAge + b.Age).Seconds()),
			History:  a.memoryLine(b.BSSID, now),
			Current:  b.BSSID == link.BSSID,
		}
		out = append(out, c)
	}
	slices.SortFunc(out, func(x, y Candidate) int { return y.RSSI - x.RSSI })
	// Keep the current AP plus the strongest N others.
	var kept []Candidate
	n := 0
	for _, c := range out {
		if c.Current {
			kept = append(kept, c)
			continue
		}
		if n < a.cfg.MaxCandidates {
			kept = append(kept, c)
			n++
		}
	}
	return kept
}

type stateLink struct {
	AP           map[string]any `json:"ap"`
	Signal       map[string]any `json:"signal"`
	Rates        map[string]any `json:"rates"`
	Quality10s   map[string]any `json:"link_quality_last_10s"`
	Quality60s   map[string]any `json:"link_quality_last_60s"`
	AppRetrans   map[string]any `json:"app_tcp_retransmits_last_10s"`
	SecondsOnAP  int            `json:"seconds_on_this_ap"`
}

func qualityMap(w linkq.Window) map[string]any {
	if !w.Valid {
		return map[string]any{"status": "no measurements"}
	}
	return map[string]any{
		"mos":        math.Round(w.MOS*100) / 100,
		"rating":     mosRating(w.MOS),
		"loss_pct":   math.Round(w.LossPct*10) / 10,
		"latency_ms": math.Round(w.LatencyMs*10) / 10,
		"jitter_ms":  math.Round(w.JitterMs*10) / 10,
	}
}

// buildState returns the JSON state object Jev evaluates.
func (a *Agent) buildState(link Link, cands []Candidate, now time.Time) map[string]any {
	// Measurements only count from when we joined the current AP; otherwise
	// the old AP's (often bad) numbers leak into the new AP's picture.
	since := func(d time.Duration) time.Time {
		t := now.Add(-d)
		if a.connChange.After(t) {
			return a.connChange
		}
		return t
	}
	w10 := a.q.Between(since(10*time.Second), now)
	w60 := a.q.Between(since(60*time.Second), now)
	var prev10 linkq.Window
	if !a.connChange.After(now.Add(-20 * time.Second)) {
		prev10 = a.q.Between(now.Add(-20*time.Second), now.Add(-10*time.Second))
	}

	cur := map[string]any{"bssid": link.BSSID, "id": apID(link.BSSID),
		"freq_mhz": link.Freq, "width": link.Width}
	for _, c := range cands {
		if c.Current {
			cur["band"] = c.Band
			cur["channel"] = c.Channel
			cur["phy"] = c.PHY
			if c.UtilPct >= 0 {
				cur["channel_utilization_pct"] = c.UtilPct
				cur["stations"] = c.Stations
			}
		}
	}

	sig := map[string]any{"rssi_dbm": link.RSSI}
	if r, ok := a.rssiAt(now.Add(-10*time.Second), a.connChange); ok {
		sig["trend_10s"] = trendWords(float64(link.RSSI-r), "dB", "10s", 3)
	}
	if r, ok := a.rssiAt(now.Add(-30*time.Second), a.connChange); ok {
		sig["trend_30s"] = trendWords(float64(link.RSSI-r), "dB", "30s", 3)
	}

	q10 := qualityMap(w10)
	if w10.Valid && prev10.Valid {
		q10["mos_vs_previous_10s"] = trendWords(
			w10.MOS-prev10.MOS, "MOS", "the previous 10s", 0.1)
	}

	retr := map[string]any{"segments_sent": w10.TCPOutSegs}
	if w10.TCPOutSegs >= 200 {
		retr["retransmit_pct"] = math.Round(w10.TCPRetransPct*10) / 10
	} else {
		retr["note"] = "too little traffic to judge"
	}

	secOnAP := int(link.Connected.Seconds())
	if !a.connChange.IsZero() {
		secOnAP = int(now.Sub(a.connChange).Seconds())
	}

	var others []map[string]any
	for _, c := range cands {
		if c.Current {
			continue
		}
		m := map[string]any{
			"id": c.ID, "band": c.Band, "channel": c.Channel,
			"width": c.Width, "phy": c.PHY, "rssi_dbm": c.RSSI,
			"rssi_vs_current_db": c.RSSIDelta,
			"est_throughput_mbps": c.EstMbps,
			"measured_seconds_ago": c.SeenAgoS,
		}
		if c.UtilPct >= 0 {
			m["channel_utilization_pct"] = c.UtilPct
		}
		if c.History != "" {
			m["history"] = c.History
		}
		others = append(others, m)
	}

	scan := map[string]any{"aps_known": len(a.scan)}
	if a.scanAt.IsZero() {
		scan["status"] = "no scan has been run yet; nearby APs are unknown"
	} else {
		scan["last_scan"] = fmt.Sprintf("%s scan %s", a.scanKind,
			ago(now.Sub(a.scanAt)))
		// Candidate RSSI is a snapshot from scan time. Say plainly how much
		// the client's own situation has changed since, which tells Jev how
		// far to trust those snapshots.
		switch {
		case a.scanBSSID != link.BSSID:
			scan["since_scan"] = "the scan was taken before the client " +
				"moved to its current AP; candidate readings are from " +
				"before that change"
		default:
			d := link.RSSI - a.scanRSSI
			switch {
			case d <= -3:
				scan["since_scan"] = fmt.Sprintf("current AP signal has "+
					"fallen %d dB since this scan; candidate readings were "+
					"taken before that change", -d)
			case d >= 3:
				scan["since_scan"] = fmt.Sprintf("current AP signal has "+
					"risen %d dB since this scan; candidate readings were "+
					"taken before that change", d)
			default:
				scan["since_scan"] = "current AP signal is about the same " +
					"as when this scan was taken"
			}
		}
	}

	roam := map[string]any{}
	if a.lastRoam.IsZero() {
		roam["last_roam"] = "none this session"
	} else {
		roam["last_roam"] = ago(now.Sub(a.lastRoam))
	}

	return map[string]any{
		"goal": "Keep the best real-time link quality for this Wi-Fi " +
			"client, measured as voice MOS to the gateway. Roaming and " +
			"scanning both briefly interrupt traffic, so changes should " +
			"be worth their cost.",
		"connection": stateLink{
			AP:     cur,
			Signal: sig,
			Rates: map[string]any{
				"tx_mcs": link.TxMCS, "rx_mcs": link.RxMCS,
				"tx_mbps": link.TxBitrate / 1_000_000,
				"rx_mbps": link.RxBitrate / 1_000_000,
			},
			Quality10s:  q10,
			Quality60s:  qualityMap(w60),
			AppRetrans:  retr,
			SecondsOnAP: secOnAP,
		},
		"candidate_aps": others,
		"scan":          scan,
		"roaming":       roam,
		"recent_events": a.recent,
	}
}

// questions builds the per-call question set. Choice options must be keys,
// so each candidate is offered under its stable ap_xxxxxx id.
func questions(cands []Candidate) map[string]jev.Question {
	targets := map[string]string{
		"none": "No candidate would give better link quality than the " +
			"current AP",
	}
	for _, c := range cands {
		if c.Current {
			continue
		}
		targets[c.ID] = fmt.Sprintf("%s ch %d, %d dBm (%+d dB vs current)",
			c.Band, c.Channel, c.RSSI, c.RSSIDelta)
	}
	return map[string]jev.Question{
		"action": jev.Choice(
			"What should the Wi-Fi client do right now to keep the best "+
				"link quality?",
			map[string]string{
				"stay": "Keep the current connection; nothing needs to " +
					"change right now",
				"scan_targeted": "Briefly scan only the channels of " +
					"known APs to refresh their measurements",
				"scan_full": "Scan every channel to discover APs that " +
					"are not known yet (longer interruption)",
				"roam": "Move to a different AP now",
			}),
		"target": jev.Choice(
			"If the client roamed now, which AP would give the best "+
				"link quality?", targets),
		"urgency": jev.Score(
			"How urgently does this client need a better connection?",
			[]string{
				"no need to change anything",
				"could be better but not pressing",
				"link is suffering and should be improved soon",
				"link is failing and needs action immediately",
			}),
		"link_degraded": jev.Noul(
			"Is the current connection's link quality degraded or " +
				"likely to degrade soon?"),
		"better_ap_available": jev.Noul(
			"Is there a candidate AP that would clearly give better " +
				"link quality than the current AP?"),
		"scan_data_stale": jev.Noul(
			"Is the scan data too old or incomplete to make a good " +
				"roaming decision?"),
	}
}

// candidateIDs maps option keys back to BSSIDs.
func candidateIDs(cands []Candidate) map[string]string {
	m := map[string]string{}
	for _, c := range cands {
		m[c.ID] = c.BSSID
	}
	return m
}
