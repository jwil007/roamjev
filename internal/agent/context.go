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
		"light steady traffic (calls, video conferencing)": "a steady " +
			"link: low loss, low latency, low jitter; every interruption " +
			"is audible",
		"heavy traffic (downloads, uploads)": "throughput: high MCS on a " +
			"wide channel with free airtime",
		"idle": "little is at stake right now, so interruptions are " +
			"cheap; a good time to improve the link before traffic resumes",
	},
	"wifi_facts": []string{
		"Band matters even at the same signal, width and load: 6 GHz " +
			"generally performs best (new, clean spectrum, wide channels, " +
			"no legacy clients), then 5 GHz. 2.4 GHz is best-effort: only " +
			"three non-overlapping channels shared by every nearby network, " +
			"plus interference from Bluetooth, microwaves and other devices. " +
			"2.4 GHz signal reads stronger at the same distance because it " +
			"propagates further, but it carries less and less reliably.",
		"Airtime is shared. A client on weak signal uses a low MCS and " +
			"takes longer to send the same data, which slows every other " +
			"client on that AP as well as itself.",
		"Channel utilization is the share of airtime already busy. A " +
			"strong signal on a very busy AP can deliver less than a " +
			"somewhat weaker signal on a quiet one.",
		"A voice call needs little throughput, so loss, latency and " +
			"jitter often stay low on weak signal until packets start " +
			"getting lost; throughput suffers much earlier.",
		"Scanning takes the radio off its home channel for each channel " +
			"scanned; nothing is sent or received meanwhile. Fewer " +
			"channels cost less. DFS channels (5 GHz 52-144) are scanned " +
			"passively and take longer each.",
		"Roaming interrupts traffic. With 802.11r fast transition (FT) " +
			"the interruption is tens of milliseconds; without FT a PSK " +
			"network needs a full handshake (about 100-250 ms) and an " +
			"802.1X network a full authentication (often 0.3-1 s, " +
			"sometimes more).",
		"Small differences in signal or estimated rate, especially " +
			"between bands, are not worth a roam. Once on a good 5 or 6 GHz " +
			"link, a client should stay unless an alternative is " +
			"substantially and lastingly better. Switching back and forth " +
			"between options is churn, not improvement.",
		"A scan of one or two channels adds one short burst of delay " +
			"(tens of ms, at most ~100 ms) that a voice call's jitter buffer " +
			"mostly absorbs: it is effectively inaudible. Longer scans repeat " +
			"those bursts for seconds, which a call can notice. While moving, a client that stops measuring other APs " +
			"ends up on a fading link with no known alternative, so brief " +
			"scans during calls are routine.",
		"When the link is degraded and no known AP is clearly better, the " +
			"client has probably moved beyond the APs it knows about. Only a " +
			"full scan can find new ones, and then it is worth its cost: a " +
			"slow scan is far better than losing the connection.",
		"Quick and known scans only re-measure known channels. " +
			"Neighboring APs often use different channels on 5 and 6 GHz, " +
			"so as the client moves, APs ahead on new channels are found " +
			"only through the AP's neighbor list (when it gives one; lists " +
			"are often incomplete) or a full scan. 2.4 GHz reuses three channels, so its " +
			"APs keep turning up in every scan.",
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

func rateWords(kbps float64) string {
	if kbps >= 1000 {
		return fmt.Sprintf("%.1f Mb/s", kbps/1000)
	}
	return fmt.Sprintf("%.0f kb/s", kbps)
}

// clientState says what the client is doing and whether it seems to move.
func (a *Agent) clientState(now time.Time) map[string]any {
	m := map[string]any{}
	if k, ok := a.trafficKbps(now, 10*time.Second); ok {
		m["traffic_last_10s"] = map[string]any{
			"rate":  rateWords(k),
			"class": trafficClass(k),
		}
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
				"%d%%; %d of %d APs above 60%% busy",
				utils[len(utils)/2], utils[len(utils)-1], busy, len(utils))
		}
		if cochan > 0 {
			m["co_channel_aps"] = fmt.Sprintf("%d other AP(s) of this "+
				"network share the current channel", cochan)
		}
	}
	m["security"] = a.securityWords(link)
	return m
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
	// Two channels: the strongest candidate's, plus the known channel that
	// has gone longest without being measured. Repeated quick scans thus
	// rotate through every known channel, keeping far-off APs (often the
	// ones the client is walking toward) fresh, while each scan stays short
	// enough (~70 ms) to be inaudible on a call. This picks channels only;
	// whether to scan is Jev's call.
	for _, c := range cands { // strongest first
		if !c.Current && c.Freq != link.Freq {
			quick = append(quick, c.Freq)
			break
		}
	}
	if f, ok := a.stalestChannel(link.Freq, quick); ok {
		quick = append(quick, f)
	}
	all := map[int]bool{link.Freq: true}
	for _, b := range a.scan {
		all[b.Freq] = true
	}
	for _, f := range a.neighbors {
		all[f] = true
	}
	for f := range all {
		known = append(known, f)
	}
	slices.Sort(quick)
	slices.Sort(known)
	return quick, known
}

func isDFS(freq int) bool { return freq >= 5260 && freq <= 5720 }

// costs states what each action costs right now, in one format.
func (a *Agent) costs(link Link, cands []Candidate) map[string]any {
	quick, known := a.scanScopes(link, cands)
	dfs := func(fs []int) int {
		n := 0
		for _, f := range fs {
			if isDFS(f) {
				n++
			}
		}
		return n
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
		wire["scan_quick"]: a.scanCost("quick", len(quick), dfs(quick)),
		wire["scan_known"]: a.scanCost("known", len(known), dfs(known)),
		wire["scan_full"]:  a.scanCost("full", 0, 0),
		"roam":             roam,
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

// The same RSSI -> MCS -> PHY rate estimate is used for the current AP and
// every candidate, so "N x the current rate" compares like with like. Noise
// floors are typical values by band (2.4 GHz is noisier); the MCS steps are
// approximate 802.11ax SNR requirements; rates assume two spatial streams.
func assumedNoise(band string) int {
	switch band {
	case "2.4GHz":
		return -89
	case "6GHz":
		return -96
	}
	return -94
}

func maxMCS(phy string) int {
	switch phy {
	case "802.11n":
		return 7
	case "802.11ac":
		return 9
	case "Legacy a/b/g":
		return 0
	}
	return 11
}

func estMCS(rssi int, band, phy string) int {
	snr := rssi - assumedNoise(band)
	steps := []int{5, 8, 11, 14, 17, 20, 23, 26, 29, 32, 35, 38}
	m := -1
	for _, s := range steps {
		if snr >= s {
			m++
		}
	}
	return max(0, min(m, maxMCS(phy)))
}

func estPHYMbps(rssi int, band, width, phy string) int {
	base := []float64{8.6, 17.2, 25.8, 34.4, 51.6, 68.8, 77.4, 86, 103.2,
		114.7, 129, 143.4} // 20 MHz, 1 stream, 802.11ax
	f := map[string]float64{"20MHz": 1, "40MHz": 2, "80MHz": 4.2,
		"160MHz": 8.4, "320MHz": 16.8}[width]
	if f == 0 {
		f = 1
	}
	return int(base[estMCS(rssi, band, phy)] * f * 2)
}

// ratio states a/b in words, which Jev reads more reliably than two numbers.
func ratio(a, b float64) string {
	if b <= 0 {
		return "unknown"
	}
	r := a / b
	switch {
	case r >= 0.9 && r <= 1.1:
		return "about the same as the current link"
	case r > 1:
		return fmt.Sprintf("about %.1fx the current link", r)
	default:
		return fmt.Sprintf("about %.0f%% of the current link", r*100)
	}
}

// trafficClass matches the briefing's what_matters_when keys exactly.
func trafficClass(kbps float64) string {
	switch {
	case kbps < 20:
		return "idle"
	case kbps < 2000:
		return "light steady traffic (calls, video conferencing)"
	default:
		return "heavy traffic (downloads, uploads)"
	}
}

// stalestChannel returns the known channel (from scans and the neighbor
// list) measured least recently, skipping the home channel and skip.
func (a *Agent) stalestChannel(home int, skip []int) (int, bool) {
	known := map[int]bool{}
	for _, b := range a.scan {
		known[b.Freq] = true
	}
	for _, f := range a.neighbors {
		known[f] = true
	}
	best, found := 0, false
	var oldest time.Time
	for f := range known {
		if f == home || slices.Contains(skip, f) {
			continue
		}
		t := a.chanScanned[f] // zero if never measured: stalest of all
		if !found || t.Before(oldest) || (t.Equal(oldest) && f < best) {
			best, oldest, found = f, t, true
		}
	}
	return best, found
}

// markScanned records when each channel was last measured.
func (a *Agent) markScanned(freqs []int, res []BSS, at time.Time) {
	if a.chanScanned == nil {
		a.chanScanned = map[int]time.Time{}
	}
	for _, f := range freqs {
		a.chanScanned[f] = at
	}
	if freqs == nil { // full scan: every channel any result came from
		for _, b := range res {
			a.chanScanned[b.Freq] = at
		}
	}
}

// scanImpact is what a scan actually did to the link, measured by the
// gateway probes: most drivers return to the home channel between scanned
// channels, so scans mostly add bursty delay (a probe sent while the radio
// is away waits for it to come back) rather than stopping traffic.
type scanImpact struct {
	dur       time.Duration
	channels  int
	lossPct   float64
	addTypMs  float64 // mean latency during the scan minus the 10 s before
	addPeakMs float64 // slowest probe during the scan minus the mean before
	valid     bool
}

func (a *Agent) measureScanImpact(kind string, channels int, from, to time.Time) {
	before := a.q.BetweenExcluding(from.Add(-10*time.Second), from, a.offChan)
	during := a.q.Between(from, to.Add(300*time.Millisecond))
	if a.scanImpact == nil {
		a.scanImpact = map[string]scanImpact{}
	}
	im := scanImpact{dur: to.Sub(from), channels: channels}
	if during.Valid && during.Sent >= 2 {
		im.valid = true
		im.lossPct = during.LossPct
		base := 0.0
		if before.Valid {
			base = before.LatencyMs
		}
		im.addTypMs = math.Max(0, during.LatencyMs-base)
		im.addPeakMs = math.Max(0, during.MaxMs-base)
	}
	a.scanImpact[kind] = im
}

// scanCost states one scan option's cost in the same form for every kind:
// channels, how long it takes, and what it did to the link last time, so
// no option looks cheaper just because it's described differently.
func (a *Agent) scanCost(kind string, channels int, dfs int) string {
	head := ""
	switch {
	case kind == "full":
		head = "every channel the radio supports"
	case channels == 0:
		return "not available until a scan has found candidate APs"
	default:
		head = fmt.Sprintf("%d channel(s)", channels)
		if dfs > 0 {
			head += fmt.Sprintf(" (%d passive DFS)", dfs)
		}
	}
	im, ok := a.scanImpact[kind]
	if ok && im.valid {
		return fmt.Sprintf("%s; last one took %.1f s, and during it gateway probes "+
			"were %.0f%% lost, latency +%.0f ms typical and up to +%.0f ms in bursts (measured)",
			head, im.dur.Seconds(), im.lossPct, im.addTypMs, im.addPeakMs)
	}
	// Not measured yet: estimate from the per-channel time of whatever
	// scans have been measured (dwell plus the return to the home channel).
	per := a.perChannelSeconds()
	n := float64(channels)
	if kind == "full" {
		n = 51
	}
	return fmt.Sprintf("%s; not measured yet, estimated about %.1f s; scans "+
		"typically add bursty latency (up to ~100 ms) while the radio visits other "+
		"channels, with little loss", head, n*per)
}

// perChannelSeconds is the measured wall-clock time per scanned channel,
// or a typical value before any scan has been measured.
func (a *Agent) perChannelSeconds() float64 {
	var secs float64
	var ch int
	for _, im := range a.scanImpact {
		if im.channels > 0 {
			secs += im.dur.Seconds()
			ch += im.channels
		}
	}
	if ch == 0 {
		return 0.2
	}
	return secs / float64(ch)
}

// envChange states how the known AP list has changed since the last full
// scan, and how many recently measured candidates are stronger than the
// current link. Facts only: deciding whether that calls for a rescan is
// Jev's job (with a safety net in agent.go if it doesn't).
func (a *Agent) envChange(link Link, cands []Candidate) map[string]any {
	// No "APs heard since the last full scan" count: only a full scan
	// can hear new APs, so it read as "nothing has changed" exactly when
	// the client had walked away from everything it knew.
	return map[string]any{
		"stronger_candidates_measured_last_30s": a.strongerFresh(cands),
	}
}

// dropDBm is roughly where a link stops carrying traffic.
const dropDBm = -85

// dropSeconds projects when a falling signal reaches dropDBm, rounded to
// 5 s; 0 unless it fell over both the last 10 s and 30 s and the drop is
// within a minute. It uses the slower of the two rates: a single 10 s
// window turns a shadowing dip at -56 dBm into "drops in 20 s", which in a
// live run drove full scans on healthy links. Jev is weak at arithmetic,
// so the code does it.
func dropSeconds(rssi, fell10s, fell30s int) int {
	if fell10s < 3 || fell30s < 3 || rssi <= dropDBm {
		return 0
	}
	rate := min(float64(fell10s)/10, float64(fell30s)/30)
	secs := float64(rssi-dropDBm) / rate
	if secs > 60 {
		return 0
	}
	return max(5, int(math.Round(secs/5))*5)
}

// situation is a one-paragraph summary of the facts that matter most for
// the scan-or-roam question, gathered from the rest of the state: where
// the signal is, the best thing the client knows about and how old that
// knowledge is, and whether it has moved since. Facts only: a speculative
// "APs near it may be ones it has never seen" made Jev full-scan, but it
// did so on healthy links too, since the sentence was true of every walk.
func (a *Agent) situation(link Link, cands []Candidate, moving bool, dropIn int) string {
	parts := []string{fmt.Sprintf("current signal %d dBm", link.RSSI)}
	var best *Candidate
	newest, known, stronger := -1, 0, 0
	for i := range cands {
		c := &cands[i]
		if c.Current {
			continue
		}
		if best == nil || c.RSSI > best.RSSI {
			best = c
		}
		known++
		if c.RSSIDelta > 0 {
			stronger++
		}
		if newest < 0 || c.SeenAgoS < newest {
			newest = c.SeenAgoS
		}
	}
	if best != nil {
		parts = append(parts, fmt.Sprintf("the strongest AP the client knows "+
			"about read %d dBm, measured %ds ago", best.RSSI, best.SeenAgoS))
		if stronger == 0 {
			parts = append(parts, fmt.Sprintf("none of the %d APs it knows "+
				"about read stronger than its current AP when measured", known))
		} else {
			parts = append(parts, fmt.Sprintf("%d of the %d APs it knows "+
				"about read stronger than its current AP when measured",
				stronger, known))
		}
	} else {
		parts = append(parts, "the client knows no other APs")
	}
	if !a.scanAt.IsZero() && a.scanBSSID != link.BSSID {
		parts = append(parts, "the client has roamed since that measurement")
	}
	if moving {
		s := "the client is moving"
		if newest >= 0 {
			s += fmt.Sprintf(" and the newest measurement of any AP it knows "+
				"is %ds old", newest)
		}
		parts = append(parts, s)
	}
	if dropIn > 0 {
		parts = append(parts, fmt.Sprintf("if the signal keeps falling at "+
			"this rate it reaches %d dBm, where connections typically drop, "+
			"in about %d s", dropDBm, dropIn))
	}
	return strings.Join(parts, "; ")
}

// strongerFresh counts candidates measured in the last 30 s whose signal
// is at least 6 dB above the current link's.
func (a *Agent) strongerFresh(cands []Candidate) int {
	n := 0
	for _, c := range cands {
		if !c.Current && c.SeenAgoS <= 30 && c.RSSIDelta >= 6 {
			n++
		}
	}
	return n
}
