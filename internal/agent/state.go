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
	RefRSSI   int     `json:"-"` // current AP reading the delta is against
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
			"last roam here %s: gateway loss %.1f%% -> %.1f%%, latency "+
				"%.0f -> %.0f ms, jitter %.1f -> %.1f ms",
			ago(now.Sub(o.T)), o.Pre.LossPct, o.Post.LossPct,
			o.Pre.LatencyMs, o.Post.LatencyMs, o.Pre.JitterMs, o.Post.JitterMs))
	}
	if m.failures > 0 {
		parts = append(parts, fmt.Sprintf(
			"%d failed roam attempt(s), latest %s: %s",
			m.failures, ago(now.Sub(m.lastFailAt)), m.lastFailMsg))
	}
	return strings.Join(parts, "; ")
}

func (a *Agent) historyLine(bssid string, now time.Time, current bool) string {
	parts := []string{}
	if !a.cfg.HideHistory {
		if h := a.historyOf(bssid, now).words(now, current); h != "" {
			parts = append(parts, h)
		}
	}
	if m := a.memoryLine(bssid, now); m != "" {
		parts = append(parts, m)
	}
	return strings.Join(parts, "; ")
}

func (a *Agent) buildCandidates(link Link, now time.Time) []Candidate {
	var out []Candidate
	scanAge := now.Sub(a.scanFetched)
	// Compare each candidate with the current AP's reading from the same
	// scan when there is one: same moment, same measurement method. The
	// live driver RSSI is a different measurement taken at a different time
	// (and the client may have moved since the scan).
	var curScan *BSS
	for i := range a.scan {
		if a.scan[i].BSSID == link.BSSID {
			curScan = &a.scan[i]
		}
	}
	ref := func(b BSS) int {
		if curScan != nil && absDur(curScan.Age-b.Age) <= 2*time.Second {
			return curScan.RSSI // same scan
		}
		// Otherwise the driver's reading of the current AP when the scan
		// that measured b ran (same moment, if not the same method).
		if a.scanBSSID == link.BSSID && absDur(now.Sub(a.scanFetched)+b.Age-
			now.Sub(a.scanAt)) <= 2*time.Second {
			return a.scanRSSI
		}
		return link.RSSI
	}
	for _, b := range a.scan {
		c := Candidate{
			ID: apID(b.BSSID), BSSID: b.BSSID, Band: b.Band,
			Channel: b.Channel, Freq: b.Freq, Width: b.Width, PHY: b.PHY,
			RSSI: b.RSSI, RSSIDelta: b.RSSI - ref(b), RefRSSI: ref(b), SNR: b.SNR,
			UtilPct: b.UtilPct, Stations: b.Stations,
			EstMbps:  b.EstThroughputKbps / 1000,
			SeenAgoS: int((scanAge + b.Age).Seconds()),
			History:  a.historyLine(b.BSSID, now, b.BSSID == link.BSSID),
			Current:  b.BSSID == link.BSSID,
		}
		out = append(out, c)
	}
	slices.SortFunc(out, func(x, y Candidate) int { return y.RSSI - x.RSSI })
	return selectCandidates(out, a.cfg.MaxCandidates)
}

// selectCandidates keeps the current AP plus up to max others, taking the
// strongest of each band first. Picking purely by RSSI let 2.4 GHz radios
// (which read stronger at the same distance) crowd 5 and 6 GHz options out
// of Jev's view. in must be sorted strongest first.
func selectCandidates(in []Candidate, max int) []Candidate {
	var kept []Candidate
	chosen := map[string]bool{}
	perBand := map[string]int{}
	pick := func(c Candidate) {
		kept = append(kept, c)
		chosen[c.BSSID] = true
	}
	for _, c := range in {
		if c.Current {
			pick(c)
		}
	}
	// Up to two per band first, strongest first.
	n := 0
	for _, c := range in {
		if c.Current || n >= max || perBand[c.Band] >= 2 {
			continue
		}
		pick(c)
		perBand[c.Band]++
		n++
	}
	// Fill the rest by signal.
	for _, c := range in {
		if n >= max {
			break
		}
		if c.Current || chosen[c.BSSID] {
			continue
		}
		pick(c)
		n++
	}
	slices.SortFunc(kept, func(x, y Candidate) int { return y.RSSI - x.RSSI })
	return kept
}

type stateLink struct {
	AP          map[string]any `json:"ap"`
	Signal      map[string]any `json:"signal"`
	Radio       map[string]any `json:"radio"`
	Gateway10s  map[string]any `json:"link_to_gateway_last_10s"`
	Gateway60s  map[string]any `json:"link_to_gateway_last_60s"`
	AppRetrans  map[string]any `json:"app_tcp_retransmits_last_10s"`
	SecondsOnAP int            `json:"seconds_on_this_ap"`
}

// gatewayMap states the measured L2 link to the gateway as plain values:
// no MOS, no ratings. The sensitivity tests showed a single rated "health"
// number anchors Jev's judgment of whether anything is wrong.
func gatewayMap(w linkq.Window) map[string]any {
	if !w.Valid {
		return map[string]any{"status": "no measurements"}
	}
	return map[string]any{
		"loss_pct":   math.Round(w.LossPct*10) / 10,
		"latency_ms": math.Round(w.LatencyMs*10) / 10,
		"jitter_ms":  math.Round(w.JitterMs*10) / 10,
		"probes":     w.Sent,
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
	// Our own scans and roams take the radio off channel; probes lost
	// then say nothing about the link, so leave those stretches out.
	w10 := a.q.BetweenExcluding(since(10*time.Second), now, a.offChan)
	w60 := a.q.BetweenExcluding(since(60*time.Second), now, a.offChan)
	var prev10 linkq.Window
	if !a.connChange.After(now.Add(-20 * time.Second)) {
		prev10 = a.q.BetweenExcluding(now.Add(-20*time.Second),
			now.Add(-10*time.Second), a.offChan)
	}

	cur := map[string]any{"bssid": link.BSSID, "id": apID(link.BSSID),
		"freq_mhz": link.Freq, "width": link.Width}
	if !a.cfg.HideHistory {
		if h := a.historyOf(link.BSSID, now).words(now, true); h != "" {
			cur["history"] = h
		}
	}
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
	if _, ok := cur["channel_utilization_pct"]; !ok && link.UtilPct > 0 {
		cur["channel_utilization_pct"] = link.UtilPct
	}

	sig := map[string]any{"rssi_dbm": link.RSSI}
	if r, ok := a.rssiAt(now.Add(-10*time.Second), a.connChange); ok {
		sig["trend_10s"] = trendWords(float64(link.RSSI-r), "dB", "10s", 3)
	}
	if r, ok := a.rssiAt(now.Add(-30*time.Second), a.connChange); ok {
		sig["trend_30s"] = trendWords(float64(link.RSSI-r), "dB", "30s", 3)
	}

	q10 := gatewayMap(w10)
	if w10.Valid && prev10.Valid {
		q10["previous_10s"] = map[string]any{
			"loss_pct":   math.Round(prev10.LossPct*10) / 10,
			"latency_ms": math.Round(prev10.LatencyMs*10) / 10,
			"jitter_ms":  math.Round(prev10.JitterMs*10) / 10,
		}
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
		est := estPHYMbps(c.RSSI, c.Band, c.Width, c.PHY)
		after := float64(est)
		if c.UtilPct >= 0 {
			after *= 1 - float64(c.UtilPct)/100
		}
		m := map[string]any{
			"id": c.ID, "band": c.Band, "channel": c.Channel,
			"width": c.Width, "phy": c.PHY, "rssi_dbm": c.RSSI,
			"rssi_vs_current_db": c.RSSIDelta,
			"est_mcs": estMCS(c.RSSI, c.Band, c.PHY),
			"est_phy_mbps":           est,
			"est_mbps_after_airtime_sharing": int(after),
			"est_rate_vs_current":    ratio(after, a.curEstAfterAt(c.RefRSSI, link, cur)),
			"measured_seconds_ago":   c.SeenAgoS,
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
	if a.lastFull.IsZero() {
		scan["last_full_scan"] = "none yet"
	} else {
		scan["last_full_scan"] = ago(now.Sub(a.lastFull))
	}
	if len(a.scan) > 0 {
		bands := map[string]bool{}
		for _, b := range a.scan {
			if b.BSSID != link.BSSID {
				bands[b.Band] = true
			}
		}
		var have []string
		for _, b := range []string{"2.4GHz", "5GHz", "6GHz"} {
			if bands[b] {
				have = append(have, b)
			}
		}
		scan["bands_with_measured_candidates"] = strings.Join(have, ", ")
	}
	for k, v := range a.envChange(link, cands) {
		scan[k] = v
	}
	if len(a.neighbors) > 0 {
		var nb []string
		for _, f := range a.neighbors {
			band, ch := bandChannel(f)
			nb = append(nb, fmt.Sprintf("%s ch %d", band, ch))
		}
		scan["neighbor_report"] = "the current AP's neighbor list (may be " +
			"incomplete) mentions " + strings.Join(nb, ", ") +
			"; a known scan covers these channels"
	} else if a.neighborsTried {
		scan["neighbor_report"] = "the current AP gave no neighbor list; " +
			"APs on channels not yet seen can only be found with a full scan"
	}
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
				"roamed to its current AP"
		default:
			d := link.RSSI - a.scanRSSI
			age := ago(now.Sub(a.scanAt))
			switch {
			case d <= -3:
				scan["since_scan"] = fmt.Sprintf("current AP signal has "+
					"fallen %d dB (from %d to %d dBm) since this scan %s",
					-d, a.scanRSSI, link.RSSI, age)
			case d >= 3:
				scan["since_scan"] = fmt.Sprintf("current AP signal has "+
					"risen %d dB (from %d to %d dBm) since this scan %s",
					d, a.scanRSSI, link.RSSI, age)
			default:
				scan["since_scan"] = fmt.Sprintf("current AP signal is "+
					"about the same as when this scan was taken %s", age)
			}
		}
	}

	roam := map[string]any{}
	if a.lastRoam.IsZero() {
		roam["last_roam"] = "none this session"
	} else {
		roam["last_roam"] = ago(now.Sub(a.lastRoam))
	}
	if !a.cfg.HideHistory {
		roam["roams_last_10_min"] = a.roamsWithin(now, historyWindow)
		if p := a.pingPong(now); p != "" {
			roam["pattern"] = p
		}
	}

	return map[string]any{
		"briefing":    briefing,
		"client":      a.clientState(now),
		"environment": a.environment(link),
		"action_costs": a.costs(link, cands),
		"connection": stateLink{
			AP:     cur,
			Signal: sig,
			Radio:      a.radioMap(link, cur),
			Gateway10s: q10,
			Gateway60s: gatewayMap(w60),
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
// so each candidate is offered under its stable ap_xxxxxx id. Action options
// use TypeSafe's structured criteria (what / not_for) to carry what each
// option is for; costs live in the state's action_costs.
func questions(cands []Candidate, verify bool) map[string]jev.Question {
	roamWhat := "Move to a different AP now, using the current candidate " +
		"measurements"
	if verify {
		roamWhat += "; the client automatically re-measures the target's " +
			"channel right before roaming (one channel, tens of ms) and " +
			"cancels if the target has faded or vanished"
	}
	targets := map[string]string{
		"none": "No candidate would give this client a better experience " +
			"than the current AP",
	}
	for _, c := range cands {
		if c.Current {
			continue
		}
		t := fmt.Sprintf("%s ch %d, %d dBm (%+d dB vs current)",
			c.Band, c.Channel, c.RSSI, c.RSSIDelta)
		if c.UtilPct >= 0 {
			t += fmt.Sprintf(", %d%% busy", c.UtilPct)
		}
		targets[c.ID] = t
	}
	return map[string]jev.Question{
		"action": {Type: "choice",
			Instructions: "Given the briefing, what the client is doing, the " +
				"environment and what each action costs, what should the " +
				"client do right now?",
			Criteria: map[string]any{
				"stay": map[string]string{
					"what":    "Keep the current connection and do nothing now",
					"not_for": "a link that is clearly failing the client's current needs",
				},
				"scan_quick": map[string]string{
					"what": "Briefly re-measure only the strongest few " +
						"candidate APs (cheapest scan)",
					"not_for": "discovering APs that are not known yet",
				},
				"scan_known": map[string]string{
					"what":    "Re-measure every known AP",
					"not_for": "discovering APs that are not known yet",
				},
				"scan_full": map[string]string{
					"what": "Sweep every channel to discover APs, including " +
						"ones not seen before (most expensive scan)",
					"not_for": "refreshing APs that are already known",
				},
				"roam": map[string]string{
					"what": roamWhat,
					"not_for": "switching back to an AP the client left " +
						"within the last minute or so, unless its situation " +
						"has clearly changed since; that is ping-pong; or " +
						"changing band for a gain of a few dB when the " +
						"current link shows no loss and the alternative's " +
						"estimated rate is lower",
				},
			}},
		"target": jev.Choice(
			"If the client roamed now, which AP would give it the best "+
				"experience for what it is doing?", targets),
		"urgency": jev.Score(
			"How urgently does this client need a better connection for "+
				"what it is doing right now?",
			[]string{
				"no need to change anything",
				"could be better but not pressing",
				"link is suffering and should be improved soon",
				"link is failing and needs action immediately",
			}),
		"link_degraded": jev.Noul(
			"Is the current connection degraded, or likely to degrade " +
				"soon, for what the client is doing?"),
		"better_ap_available": jev.Noul(
			"Is there a candidate AP that would clearly give this client " +
				"a better experience than the current AP?"),
		"roam_short_lived": jev.Noul(
			"If the client roamed now, would that roam likely be " +
				"short-lived or reversed soon?"),
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

func curUtil(cur map[string]any) int {
	if u, ok := cur["channel_utilization_pct"].(int); ok {
		return u
	}
	return 0
}

// curEstAfter is the current AP's estimated rate after airtime sharing,
// from the same estimator the candidates use.
func (a *Agent) curEstAfter(link Link, cur map[string]any) float64 {
	return a.curEstAfterAt(link.RSSI, link, cur)
}

// curEstAfterAt estimates the current AP's rate at a given RSSI reading.
func (a *Agent) curEstAfterAt(rssi int, link Link, cur map[string]any) float64 {
	band, _ := cur["band"].(string)
	phy, _ := cur["phy"].(string)
	if band == "" {
		band, _ = bandChannel(link.Freq)
	}
	est := float64(estPHYMbps(rssi, band, link.Width, phy))
	if u := curUtil(cur); u > 0 {
		est *= 1 - float64(u)/100
	}
	return est
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// radioMap is how the radio link is actually performing (reported by the
// driver) next to the same RSSI-based estimate used for candidates.
func (a *Agent) radioMap(link Link, cur map[string]any) map[string]any {
	band, _ := cur["band"].(string)
	phy, _ := cur["phy"].(string)
	if band == "" {
		band, _ = bandChannel(link.Freq)
	}
	m := map[string]any{
		"band": band, "width": link.Width,
		"rx_mcs":         fmt.Sprintf("%d (max %d for %s)", link.RxMCS, maxMCS(phy), orUnknown(phy)),
		"tx_mcs":         link.TxMCS,
		"rx_phy_mbps":    link.RxBitrate / 1_000_000,
		"tx_phy_mbps":    link.TxBitrate / 1_000_000,
		"est_phy_mbps":   estPHYMbps(link.RSSI, band, link.Width, phy),
		"est_mbps_after_airtime_sharing": int(a.curEstAfter(link, cur)),
	}
	if u := curUtil(cur); u > 0 {
		m["channel_utilization_pct"] = u
		m["rx_mbps_after_airtime_sharing"] = effMbps(link.RxBitrate, u)
	}
	return m
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown PHY"
	}
	return s
}
