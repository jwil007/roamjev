package agent

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// This file builds the parts of Jev's state that let it adapt to the
// environment instead of applying one fixed policy everywhere: a briefing
// that states the goal and the Wi-Fi tradeoffs (Jev has no system prompt, so
// this rides along in every call), what each action costs here (measured),
// what the environment looks like, and what the client is doing.
//
// Everything is description. None of it is a threshold that triggers an
// action; deciding is Jev's job.

// briefing is the standing context. TypeSafe's guidance is to put the
// relevant policy and knowledge in the state next to the data
// (https://docs.typesafe.ai/concepts/system-one.md); this is that policy.
var briefing = map[string]any{
	"goal": "Give this Wi-Fi client the best experience for what it is " +
		"doing right now, at the lowest cost to the network.",
	"what_matters_when": map[string]string{
		"on a call or other real-time traffic": "steady, low-loss, " +
			"low-jitter link (voice MOS); every interruption is audible",
		"downloading or other heavy traffic": "throughput: a strong " +
			"signal (high MCS) on an AP with free airtime",
		"idle": "little is at stake right now, so interruptions are " +
			"cheap; a good time to improve the link before traffic resumes",
	},
	"wifi_facts": []string{
		"Airtime is shared. A client on weak signal uses a low MCS and " +
			"takes longer to send the same data, which slows every other " +
			"client on that AP as well as itself.",
		"Channel utilization is the share of airtime already busy. A " +
			"strong signal on a very busy AP can deliver less than a " +
			"somewhat weaker signal on a quiet one.",
		"A voice call needs little throughput, so voice quality often " +
			"stays good on weak signal until packets start getting lost; " +
			"throughput suffers much earlier.",
		"Scanning takes the radio off its home channel for each channel " +
			"scanned; nothing is sent or received meanwhile. Fewer " +
			"channels cost less. DFS channels (5 GHz 52-144) are scanned " +
			"passively and take longer each.",
		"Roaming interrupts traffic. With 802.11r fast transition (FT) " +
			"the interruption is tens of milliseconds; without FT a PSK " +
			"network needs a full handshake (about 100-250 ms) and an " +
			"802.1X network a full authentication (often 0.3-1 s, " +
			"sometimes more).",
		"Signal readings from a scan are snapshots; if the client is " +
			"moving they drift within seconds. Waiting for perfectly fresh " +
			"readings while moving can mean never acting: a reading a few " +
			"seconds old is usually good enough when the difference between " +
			"APs is large, and every rescan costs airtime too.",
	},
}

type counterPoint struct {
	t      time.Time
	rx, tx uint64
}

// trafficKbps is the interface rate over the last d.
func (a *Agent) trafficKbps(now time.Time, d time.Duration) (float64, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.counters) < 2 {
		return 0, false
	}
	last := a.counters[len(a.counters)-1]
	first := last
	for i := len(a.counters) - 1; i >= 0; i-- {
		if now.Sub(a.counters[i].t) > d {
			break
		}
		first = a.counters[i]
	}
	secs := last.t.Sub(first.t).Seconds()
	if secs < 1 || last.rx < first.rx || last.tx < first.tx {
		return 0, false
	}
	bits := float64((last.rx-first.rx)+(last.tx-first.tx)) * 8
	return bits / secs / 1000, true
}

func trafficWords(kbps float64) string {
	switch {
	case kbps < 20:
		return "idle"
	case kbps < 500:
		return fmt.Sprintf("light and steady, about %.0f kb/s (typical of "+
			"a voice call or chat)", kbps)
	case kbps < 5000:
		return fmt.Sprintf("moderate, about %.1f Mb/s", kbps/1000)
	default:
		return fmt.Sprintf("heavy, about %.0f Mb/s (bulk transfer)", kbps/1000)
	}
}

// clientState says what the client is doing and whether it seems to move.
func (a *Agent) clientState(now time.Time) map[string]any {
	m := map[string]any{}
	if k, ok := a.trafficKbps(now, 10*time.Second); ok {
		m["traffic_last_10s"] = trafficWords(k)
	} else {
		m["traffic_last_10s"] = "not measured yet"
	}
	// Motion from signal swing on the current AP over the last 30 s.
	a.mu.Lock()
	var lo, hi, n int
	for _, p := range a.rssiHis {
		if now.Sub(p.t) > 30*time.Second || p.t.Before(a.connChange) {
			continue
		}
		if n == 0 || p.rssi < lo {
			lo = p.rssi
		}
		if n == 0 || p.rssi > hi {
			hi = p.rssi
		}
		n++
	}
	a.mu.Unlock()
	switch {
	case n < 10:
		m["motion"] = "not enough signal history on this AP yet"
	case hi-lo >= 8:
		m["motion"] = fmt.Sprintf("probably moving: signal swung %d dB "+
			"over the last 30s", hi-lo)
	case hi-lo <= 4:
		m["motion"] = fmt.Sprintf("probably stationary: signal within "+
			"%d dB over the last 30s", hi-lo)
	default:
		m["motion"] = fmt.Sprintf("unclear: signal varied %d dB over "+
			"the last 30s", hi-lo)
	}
	return m
}

// environment summarizes the RF neighborhood from the latest scan.
func (a *Agent) environment(link Link) map[string]any {
	m := map[string]any{}
	if len(a.scan) == 0 {
		m["status"] = "unknown until a scan has run"
	} else {
		bands := map[string]int{}
		var utils []int
		busy, cochan := 0, 0
		for _, b := range a.scan {
			bands[b.Band]++
			if b.UtilPct >= 0 {
				utils = append(utils, b.UtilPct)
				if b.UtilPct >= 60 {
					busy++
				}
			}
			if b.Freq == link.Freq && b.BSSID != link.BSSID {
				cochan++
			}
		}
		var parts []string
		for _, band := range []string{"2.4GHz", "5GHz", "6GHz"} {
			if bands[band] > 0 {
				parts = append(parts, fmt.Sprintf("%d on %s", bands[band], band))
			}
		}
		m["aps_for_this_network"] = fmt.Sprintf("%d (%s)", len(a.scan),
			strings.Join(parts, ", "))
		if len(utils) > 0 {
			slices.Sort(utils)
			m["channel_utilization"] = fmt.Sprintf("median %d%%, busiest "+
				"%d%%; %d of %d APs above 60%% busy (%s)",
				utils[len(utils)/2], utils[len(utils)-1], busy, len(utils),
				loadWords(utils[len(utils)/2]))
		}
		if cochan > 0 {
			m["co_channel_aps"] = fmt.Sprintf("%d other AP(s) of this "+
				"network share the current channel", cochan)
		}
	}
	m["security"] = a.securityWords(link)
	return m
}

func loadWords(median int) string {
	switch {
	case median >= 60:
		return "a busy, high-density environment"
	case median >= 35:
		return "moderately busy"
	default:
		return "lightly loaded"
	}
}

func (a *Agent) securityWords(link Link) string {
	km := link.KeyMgmt
	ft := strings.HasPrefix(km, "FT-")
	for _, b := range a.scan {
		if b.BSSID == link.BSSID && b.FT {
			ft = true
		}
	}
	switch {
	case km == "":
		return "unknown"
	case ft:
		return km + ": 802.11r fast transition is in use, so roams are quick"
	case strings.Contains(km, "802.1X") || strings.Contains(km, "EAP"):
		return km + ": no fast transition, so every roam does a full " +
			"802.1X authentication (slow)"
	default:
		return km + ": no fast transition, so every roam does a full handshake"
	}
}

// scanScopes returns the channel list for each scan action.
func (a *Agent) scanScopes(link Link, cands []Candidate) (quick, known []int) {
	seen := map[int]bool{}
	for _, c := range cands { // cands are strongest first
		if c.Current || seen[c.Freq] {
			continue
		}
		seen[c.Freq] = true
		quick = append(quick, c.Freq)
		if len(quick) == 3 {
			break
		}
	}
	all := map[int]bool{link.Freq: true}
	for _, b := range a.scan {
		all[b.Freq] = true
	}
	for f := range all {
		known = append(known, f)
	}
	slices.Sort(quick)
	slices.Sort(known)
	return quick, known
}

func isDFS(freq int) bool { return freq >= 5260 && freq <= 5720 }

func (a *Agent) scanCostWords(freqs []int) string {
	if len(freqs) == 0 {
		return "not available until a scan has found candidate APs"
	}
	dfs := 0
	for _, f := range freqs {
		if isDFS(f) {
			dfs++
		}
	}
	ms := a.estimateScanMs(freqs)
	s := fmt.Sprintf("%d channel(s), about %.0f ms off the home channel",
		len(freqs), ms)
	if dfs > 0 {
		s += fmt.Sprintf(" (%d passive DFS)", dfs)
	}
	return s
}

// estimateScanMs uses measured per-channel dwell when available, split into
// active and passive (DFS) channels.
func (a *Agent) estimateScanMs(freqs []int) float64 {
	active, passive := a.dwellActive, a.dwellPassive
	if active == 0 {
		active = 35
	}
	if passive == 0 {
		passive = 105
	}
	var ms float64
	for _, f := range freqs {
		if isDFS(f) {
			ms += passive
		} else {
			ms += active
		}
	}
	return ms
}

// learnScanCost updates the dwell estimates from a finished scan. A scan
// mixing DFS and non-DFS channels can't separate the two, so the passive
// estimate keeps a fixed 3x ratio to the active one.
func (a *Agent) learnScanCost(freqs []int, dur time.Duration) {
	ms := float64(dur.Microseconds()) / 1000
	if freqs == nil {
		a.fullScanMs = ewma(a.fullScanMs, ms)
		return
	}
	units := 0.0
	for _, f := range freqs {
		if isDFS(f) {
			units += 3
		} else {
			units++
		}
	}
	if units == 0 {
		return
	}
	a.dwellActive = ewma(a.dwellActive, ms/units)
	a.dwellPassive = 3 * a.dwellActive
}

func ewma(old, x float64) float64 {
	if old == 0 {
		return x
	}
	return 0.7*old + 0.3*x
}

// costs states what each action costs right now.
func (a *Agent) costs(link Link, cands []Candidate) map[string]any {
	quick, known := a.scanScopes(link, cands)
	full := "a sweep of every channel"
	if a.fullScanMs > 0 {
		full += fmt.Sprintf(", about %.1f s off the home channel (measured)",
			a.fullScanMs/1000)
	} else {
		full += ", typically 2-4 s off the home channel"
	}
	roam := "unknown"
	if len(a.roamDurs) > 0 {
		d := slices.Clone(a.roamDurs)
		slices.Sort(d)
		roam = fmt.Sprintf("about %d ms interruption (median of the last "+
			"%d roams, range %d-%d ms)", d[len(d)/2], len(d), d[0], d[len(d)-1])
	} else {
		roam = "no roams measured yet; " + a.securityWords(link)
	}
	return map[string]any{
		"scan_quick": a.scanCostWords(quick) + "; refreshes the strongest " +
			"few candidates only",
		"scan_known": a.scanCostWords(known) + "; refreshes every known AP",
		"scan_full":  full + "; also finds APs not seen before",
		"roam":       roam,
	}
}

// effMbps is the PHY rate scaled by free airtime: roughly what the link
// could deliver to this client.
func effMbps(rxBitrate, utilPct int) float64 {
	r := float64(rxBitrate) / 1e6
	if utilPct > 0 {
		r *= 1 - float64(utilPct)/100
	}
	return math.Round(r)
}
