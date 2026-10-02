// Package sim fakes Wi-Fi environments so the agent and UI can be exercised
// end to end (with real Jev calls) without touching a radio. Each scenario
// (see scenarios.go) defines AP layout, load, security, the client's path
// and what it's doing. RSSI follows log-distance path loss with slow
// shadowing; link quality degrades with low SNR and channel load. Scans take
// the radio off its home channel for each channel's dwell, and roams
// interrupt traffic for a time set by the network's security, so both cost
// what they cost on a real link.
package sim

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/jwil007/roamjev/internal/agent"
	"github.com/jwil007/roamjev/internal/linkq"
)

// Noise floors by band. 2.4 GHz is crowded with Bluetooth, microwaves,
// cordless devices and every nearby network on just three channels; 6 GHz is
// new spectrum with no legacy clients. On top of its floor, each 2.4 GHz
// radio suffers random interference bursts (see interference).
func noiseFloor(band string) float64 {
	switch band {
	case "2.4GHz":
		return -89
	case "6GHz":
		return -96
	}
	return -94
}

type ap struct {
	idx     int // physical AP index in the scenario
	shared  *float64 // shadowing shared by all radios of this AP (same path)
	bssid   string
	x, y    float64
	freq    int
	channel int
	band    string
	width   string
	phy     string
	txPower float64
	pl1m    float64
	util    float64
	shadow  float64
	burst   float64   // extra noise (dB) from a 2.4 GHz interference burst
	burstTo time.Time // burst end
}

// noise is the AP's current noise floor including any burst.
func (a *ap) noise() float64 { return noiseFloor(a.band) + a.burst }

type World struct {
	SSID string
	// Seed drives two independent streams: wr for the world itself (walk,
	// pauses, shadowing, load, activity) and nr for measurement noise and
	// roam luck. Two runs with the same seed see the identical world,
	// whatever their deciders do.
	Seed uint64
	sc   *scenario
	wr   *rand.Rand
	nr   *rand.Rand

	mu       sync.Mutex
	aps      []*ap
	x, y     float64
	wp       int       // waypoint being walked to (or paused at)
	pauseTil time.Time // paused until
	arrived  bool
	cur      *ap
	since    time.Time
	busy     string // "scan" or "roam" while the radio is off channel
	cache    map[string]agent.BSS
	cacheAt  map[string]time.Time
	start    time.Time
	ring     *linkq.Ring
	down     bool
	tcpOut   uint64
	tcpRetx  uint64
	rxBytes  float64
	txBytes  float64
	activity string
	actTil   time.Time
	// cf holds, per AP, the probes the client would have measured had it
	// stayed associated there: the counterfactual a roam is graded against.
	cf map[string]*linkq.Ring
}

// NewWorld builds a scenario. speed overrides the scenario's walking speed
// when > 0.
func NewWorld(ring *linkq.Ring, speed float64, scenario string, seed uint64) (*World, error) {
	sc, err := scenarioByName(scenario)
	if err != nil {
		return nil, err
	}
	if seed == 0 {
		seed = uint64(time.Now().UnixNano())
	}
	if speed > 0 && sc.speed > 0 {
		sc.speed = speed
	}
	w := &World{SSID: "lab-sim", Seed: seed, sc: sc, ring: ring,
		wr:    rand.New(rand.NewPCG(seed, 1)),
		nr:    rand.New(rand.NewPCG(seed, 2)),
		cache: map[string]agent.BSS{}, cacheAt: map[string]time.Time{},
		start: time.Now(), cf: map[string]*linkq.Ring{}}
	for i, s := range sc.aps {
		shared := new(float64)
		for ri, r := range s.radios {
			freq := chanFreq(r.band, r.channel)
			pl := 46.4 // 5 GHz free-space loss at 1 m
			switch r.band {
			case "2.4GHz":
				pl = 40.0
			case "6GHz":
				pl = 47.4 // ~1 dB more free-space loss than 5 GHz
			}
			a := &ap{idx: i, shared: shared,
				// Radios of one AP share a base MAC and differ in the last
				// octet, as most vendors allocate them.
				bssid: fmt.Sprintf("02:5a:00:%02x:%02x:%02x", i+1, 0x10,
					0x10*ri),
				x: s.x, y: s.y, freq: freq, channel: r.channel, band: r.band,
				width: r.width, phy: r.phy, txPower: r.txPower, pl1m: pl,
				util: s.baseUtil}
			w.aps = append(w.aps, a)
			w.cf[a.bssid] = linkq.NewRing(5 * time.Minute)
		}
	}
	w.x, w.y = sc.path[0].x, sc.path[0].y
	w.wp, w.arrived = 0, true
	w.pauseTil = w.start.Add(w.pause(sc.path[0]))
	// Start on the strongest AP, like a fresh association.
	w.cur = w.aps[0]
	for _, a := range w.aps {
		if w.rssiOf(a) > w.rssiOf(w.cur) {
			w.cur = a
		}
	}
	w.since = w.start
	w.nextActivity(w.start)
	return w, nil
}

func chanFreq(band string, ch int) int {
	switch band {
	case "2.4GHz":
		return 2407 + 5*ch
	case "6GHz":
		return 5950 + 5*ch
	}
	return 5000 + 5*ch
}

func (w *World) pause(p waypoint) time.Duration {
	s := p.pauseMin + w.wr.Float64()*(p.pauseMax-p.pauseMin)
	return time.Duration(s * float64(time.Second))
}

func (w *World) nextActivity(now time.Time) {
	var total float64
	for _, p := range w.sc.traffic {
		total += p.weight
	}
	r := w.wr.Float64() * total
	for _, p := range w.sc.traffic {
		if r -= p.weight; r <= 0 {
			w.activity = p.kind
			d := p.minS + w.wr.Float64()*(p.maxS-p.minS)
			w.actTil = now.Add(time.Duration(d * float64(time.Second)))
			return
		}
	}
}

func (w *World) rssiOf(a *ap) float64 {
	d := math.Max(1, math.Hypot(a.x-w.x, a.y-w.y))
	return a.txPower - (a.pl1m + 10*w.sc.exponent*math.Log10(d)) + *a.shared + a.shadow
}

// Run advances the world and generates ARP-like probe samples into the ring.
func (w *World) Run(ctx context.Context) {
	step := 250 * time.Millisecond
	t := time.NewTicker(step)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			w.mu.Lock()
			w.advance(now, step.Seconds())
			w.probeCounterfactuals(now)
			s, out, retx := w.probe(now)
			w.traffic(step.Seconds())
			w.tcpOut += out
			w.tcpRetx += retx
			tcp := linkq.TCPSample{At: now, OutSegs: w.tcpOut,
				RetransSegs: w.tcpRetx}
			w.mu.Unlock()
			w.ring.Add(s)
			w.ring.AddTCP(tcp)
		}
	}
}

func (w *World) advance(now time.Time, dt float64) {
	sc := w.sc
	// Movement along the waypoint loop, with planned stops at waypoints and
	// occasional unplanned ones along the way.
	if sc.speed > 0 && now.After(w.pauseTil) {
		if w.arrived {
			w.wp = (w.wp + 1) % len(sc.path)
			w.arrived = false
		}
		tgt := sc.path[w.wp]
		dx, dy := tgt.x-w.x, tgt.y-w.y
		dist := math.Hypot(dx, dy)
		stepLen := sc.speed * dt
		if dist <= stepLen {
			w.x, w.y = tgt.x, tgt.y
			w.arrived = true
			w.pauseTil = now.Add(w.pause(tgt))
		} else {
			w.x += dx / dist * stepLen
			w.y += dy / dist * stepLen
			if sc.pauseEvery > 0 && w.wr.Float64() < dt/sc.pauseEvery {
				w.pauseTil = now.Add(time.Duration(5+w.wr.IntN(20)) * time.Second)
			}
		}
	}
	if now.After(w.actTil) {
		w.nextActivity(now)
	}
	el := now.Sub(w.start)
	stepped := map[*float64]bool{}
	for _, a := range w.aps {
		if !stepped[a.shared] {
			*a.shared += (w.wr.Float64()-0.5)*sc.shadow - *a.shared*0.02
			stepped[a.shared] = true
		}
		a.shadow += (w.wr.Float64()-0.5)*0.3 - a.shadow*0.05
		target := sc.aps[a.idx].baseUtil + sc.hot(a.idx, el)
		switch a.band {
		case "2.4GHz":
			target += 15
		case "6GHz":
			target -= 12
		}
		a.util += (target-a.util)*0.05 + (w.wr.Float64()-0.5)*2
		a.util = math.Max(1, math.Min(99, a.util))
		// 2.4 GHz: a burst roughly every minute, lasting 3-15 s, raising
		// the noise floor 6-15 dB (a microwave, a Bluetooth speaker, a
		// neighbor's camera).
		if a.band == "2.4GHz" {
			if a.burst > 0 && now.After(a.burstTo) {
				a.burst = 0
			}
			if a.burst == 0 && w.wr.Float64() < dt/60 {
				a.burst = 6 + w.wr.Float64()*9
				a.burstTo = now.Add(time.Duration(3+w.wr.IntN(12)) * time.Second)
			}
		}
	}
	if w.down {
		return
	}
	// Below the sensitivity floor the association drops; like
	// wpa_supplicant, reconnect to the strongest AP a few seconds later.
	if w.rssiOf(w.cur) < -88 && w.busy == "" {
		w.down = true
		go func() {
			time.Sleep(3 * time.Second)
			w.mu.Lock()
			defer w.mu.Unlock()
			best := w.aps[0]
			for _, a := range w.aps {
				if w.rssiOf(a) > w.rssiOf(best) {
					best = a
				}
			}
			w.cur, w.since, w.down = best, time.Now(), false
		}()
	}
}

// traffic accumulates interface bytes for the current activity. A download
// takes what the link can carry; nothing moves while off channel.
func (w *World) traffic(dt float64) {
	if w.down || w.busy != "" {
		w.rxBytes += 50 * dt
		return
	}
	switch w.activity {
	case "idle":
		w.rxBytes += 300 * dt
		w.txBytes += 200 * dt
	case "call":
		w.rxBytes += 12_500 * dt // ~100 kb/s each way
		w.txBytes += 12_500 * dt
	case "download":
		mbps := math.Min(w.effMbps(w.cur), 300)
		w.rxBytes += mbps * 1e6 / 8 * dt
		w.txBytes += mbps * 1e6 / 8 * dt / 40
	}
}

// effMbps is what the link can actually deliver: PHY rate times the
// airtime left over by other clients, times a MAC efficiency factor.
func (w *World) effMbps(a *ap) float64 {
	snr := w.rssiOf(a) - a.noise()
	return float64(rateFor(mcsFor(snr), a.width)) / 1e6 *
		(1 - a.util/100) * 0.6
}

func (w *World) probe(now time.Time) (linkq.Sample, uint64, uint64) {
	if w.down || w.busy == "roam" {
		return linkq.Sample{At: now, Lost: true}, 0, 0
	}
	return w.probeAP(now, w.cur, w.busy == "scan")
}

// probeCounterfactuals samples every AP as if the client were associated to
// it and not scanning. Below the sensitivity floor it would have dropped.
func (w *World) probeCounterfactuals(now time.Time) {
	for _, a := range w.aps {
		s := linkq.Sample{At: now, Lost: true}
		if w.rssiOf(a) >= -88 {
			s, _, _ = w.probeAP(now, a, false)
		}
		w.cf[a.bssid].Add(s)
	}
}

// StayQuality implements agent.Counterfactual.
func (w *World) StayQuality(bssid string, from, to time.Time) (linkq.Window, bool) {
	w.mu.Lock()
	r := w.cf[bssid]
	w.mu.Unlock()
	if r == nil {
		return linkq.Window{}, false
	}
	win := r.Between(from, to)
	return win, win.Valid
}

func (w *World) probeAP(now time.Time, a *ap, scanning bool) (linkq.Sample, uint64, uint64) {
	s := linkq.Sample{At: now}
	snr := w.rssiOf(a) + w.nr.NormFloat64()*1.5 - a.noise()
	util := a.util
	pLoss := 0.5/(1+math.Exp((snr-12)/2.2)) + math.Max(0, util-60)/40*0.08
	if scanning {
		// Off the home channel for most of a scan.
		pLoss += 0.8
	}
	lat := 1.5 + math.Max(0, 25-snr)*0.8 + math.Max(0, util-40)*0.25
	lat += math.Abs(w.nr.NormFloat64()) * (1 + math.Max(0, util-50)*0.12 +
		math.Max(0, 22-snr)*0.4)
	out := uint64(40 + w.nr.IntN(20))
	retx := uint64(float64(out) * math.Min(0.5, pLoss*1.5))
	if w.nr.Float64() < pLoss {
		s.Lost = true
		return s, out, retx
	}
	s.RTT = time.Duration(lat * float64(time.Millisecond))
	return s, out, retx
}

func mcsFor(snr float64) int {
	steps := []float64{5, 8, 11, 14, 17, 20, 23, 26, 29, 32, 35, 38}
	m := -1
	for _, s := range steps {
		if snr >= s {
			m++
		}
	}
	return max(0, m)
}

func rateFor(mcs int, width string) int {
	base := []float64{8.6, 17.2, 25.8, 34.4, 51.6, 68.8, 77.4, 86, 103.2,
		114.7, 129, 143.4} // 802.11ax 20 MHz, 1SS, 0.8 us GI
	f := map[string]float64{"20MHz": 1, "40MHz": 2, "80MHz": 4.2,
		"160MHz": 8.4}[width]
	return int(base[min(mcs, 11)] * f * 2 * 1e6)
}

func (w *World) keyMgmt() string {
	switch {
	case w.sc.security == "802.1X/EAP" && w.sc.ft:
		return "FT-EAP"
	case w.sc.security == "802.1X/EAP":
		return "WPA2/IEEE 802.1X/EAP"
	case w.sc.ft:
		return "FT-PSK"
	}
	return "WPA2-PSK"
}

func (w *World) Link(_ context.Context) (agent.Link, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.down {
		return agent.Link{SSID: w.SSID, WPAState: "DISCONNECTED"}, nil
	}
	rssi := w.rssiOf(w.cur) + w.nr.NormFloat64()
	m := mcsFor(rssi - w.cur.noise())
	return agent.Link{
		SSID: w.SSID, BSSID: w.cur.bssid, Freq: w.cur.freq,
		WPAState: "COMPLETED", RSSI: int(math.Round(rssi)),
		TxMCS: max(0, m-1), RxMCS: m,
		TxBitrate: rateFor(max(0, m-1), w.cur.width),
		RxBitrate: rateFor(m, w.cur.width),
		TxPHY:     "HE", RxPHY: "HE", Width: w.cur.width,
		Connected: time.Since(w.since),
		KeyMgmt:   w.keyMgmt(),
		UtilPct:   int(w.cur.util),
	}, nil
}

func (w *World) Scan(ctx context.Context, freqs []int) error {
	list := freqs
	if list == nil {
		list = fullScanFreqs()
	}
	var ms float64
	for _, f := range list {
		ms += dwellMs(f)
	}
	w.mu.Lock()
	w.busy = "scan"
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.busy = ""
		w.mu.Unlock()
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Duration(ms * float64(time.Millisecond))):
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	want := map[int]bool{}
	for _, f := range list {
		want[f] = true
	}
	now := time.Now()
	for _, a := range w.aps {
		if !want[a.freq] {
			continue
		}
		rssi := w.rssiOf(a) + w.nr.NormFloat64()*2
		if rssi < -88 {
			delete(w.cache, a.bssid)
			continue
		}
		w.cache[a.bssid] = agent.BSS{
			BSSID: a.bssid, SSID: w.SSID, Freq: a.freq, Channel: a.channel,
			Band: a.band, RSSI: int(math.Round(rssi)),
			SNR: int(math.Round(rssi - a.noise())), Width: a.width,
			PHY: a.phy, UtilPct: int(a.util), Stations: 3 + int(a.util/4),
			EstThroughputKbps: rateFor(mcsFor(rssi-a.noise()), a.width) / 1000,
			Security:          w.sc.security, FT: w.sc.ft,
		}
		w.cacheAt[a.bssid] = now
	}
	return nil
}

func (w *World) ScanResults(_ context.Context, _ string) ([]agent.BSS, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []agent.BSS
	for k, b := range w.cache {
		b.Age = time.Since(w.cacheAt[k])
		out = append(out, b)
	}
	slices.SortFunc(out, func(a, b agent.BSS) int { return b.RSSI - a.RSSI })
	return out, nil
}

func (w *World) Roam(ctx context.Context, bssid string) (agent.RoamResult, error) {
	w.mu.Lock()
	var target *ap
	for _, a := range w.aps {
		if a.bssid == bssid {
			target = a
		}
	}
	if target == nil {
		w.mu.Unlock()
		return agent.RoamResult{Message: "BSSID not found"}, nil
	}
	w.busy = "roam"
	rssi := w.rssiOf(target)
	d := time.Duration(w.sc.roamMs(w.nr) * float64(time.Millisecond))
	fail := rssi < -82 || w.nr.Float64() < 0.03
	w.mu.Unlock()
	select {
	case <-ctx.Done():
		return agent.RoamResult{}, ctx.Err()
	case <-time.After(d):
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.busy = ""
	if fail {
		return agent.RoamResult{Duration: d,
			Message: "Assoc rejected - Association denied, poor channel " +
				"conditions"}, nil
	}
	w.cur, w.since = target, time.Now()
	return agent.RoamResult{Success: true, Final: bssid, Duration: d}, nil
}

func (w *World) Prepare() (func(), error) { return func() {}, nil }

func (w *World) Counters() (uint64, uint64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return uint64(w.rxBytes), uint64(w.txBytes), nil
}

// Activity implements agent.ActivityReporter (grading only).
func (w *World) Activity() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.activity
}

// Position reports the client's position and the scenario, for the UI.
func (w *World) Position() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return fmt.Sprintf("simulated %s (client at %.0f, %.0f m; %s)",
		w.sc.name, w.x, w.y, w.activity)
}

// Describe summarizes the scenario.
func (w *World) Describe() string { return w.sc.describe }

// NeighborFreqs implements agent.NeighborReporter: the current AP's 802.11k
// neighbor report lists the channels of APs within about 60 m of it (and
// its own other radios), as an enterprise controller would.
func (w *World) NeighborFreqs(_ context.Context) ([]int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.sc.nrCoverage == 0 {
		return nil, fmt.Errorf("NEIGHBOR_REP_REQUEST: FAIL (AP may not support 802.11k)")
	}
	seen := map[int]bool{}
	var out []int
	for _, a := range w.aps {
		if a == w.cur || math.Hypot(a.x-w.cur.x, a.y-w.cur.y) > 60 {
			continue
		}
		// Incomplete lists: whether this AP lists that radio is fixed per
		// pair (a stable, imperfect neighbor table, not a coin flip each time).
		if pairHash(w.cur.bssid, a.bssid) >= w.sc.nrCoverage {
			continue
		}
		if !seen[a.freq] {
			seen[a.freq] = true
			out = append(out, a.freq)
		}
	}
	slices.Sort(out)
	return out, nil
}

// pairHash maps a pair of BSSIDs to a stable value in [0, 1).
func pairHash(a, b string) float64 {
	var h uint64 = 1469598103934665603
	for _, c := range a + "|" + b {
		h ^= uint64(c)
		h *= 1099511628211
	}
	return float64(h%10000) / 10000
}
