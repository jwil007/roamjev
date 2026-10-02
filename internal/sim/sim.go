// Package sim fakes a Wi-Fi environment so the agent and UI can be exercised
// end to end (with real Jev calls) without touching a radio. A client walks
// a hallway lined with APs; RSSI follows log-distance path loss with slow
// shadowing, and link quality degrades with low SNR and channel load. Scans
// and roams cost probe loss, just as they cost airtime on a real link.
package sim

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/jwil007/roamjev/internal/agent"
	"github.com/jwil007/roamjev/internal/linkq"
)

const noiseFloor = -95

type ap struct {
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
	ft      bool
}

type World struct {
	SSID  string
	Speed float64 // walking speed, m/s
	// Scenario is "walk" (hallway of APs) or "boundary" (standing still
	// midway between two equal APs: the classic ping-pong setup).
	Scenario string
	// shadowStep scales the slow shadowing random walk.
	shadowStep float64

	mu       sync.Mutex
	aps      []*ap
	x        float64
	dir      float64
	pauseTil time.Time
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
	// cf holds, per AP, the probes the client would have measured had it
	// stayed associated there: the counterfactual a roam is graded against.
	cf map[string]*linkq.Ring
}

func NewWorld(ring *linkq.Ring, speed float64, scenario string) (*World, error) {
	w := &World{SSID: "lab-sim", Speed: speed, ring: ring, dir: 1,
		cache: map[string]agent.BSS{}, cacheAt: map[string]time.Time{},
		start: time.Now(), x: 2, Scenario: scenario, shadowStep: 0.6}
	add := func(i int, x float64, freq, ch int, band, width, phy string,
		tx, pl float64) {
		w.aps = append(w.aps, &ap{
			bssid: fmt.Sprintf("02:5a:%02x:00:%02x:%02x", i, freq%256, ch),
			x:     x, y: 4, freq: freq, channel: ch, band: band, width: width,
			phy: phy, txPower: tx, pl1m: pl, util: 15 + rand.Float64()*20,
			ft: true,
		})
	}
	if scenario == "boundary" {
		// Two identical 5 GHz APs 90 m apart; the client stands midway at
		// about -76 dBm from each: the marginal edge where real ping-pong
		// happens. Shadowing (~3 dB) keeps swapping which one looks
		// stronger, and dips on either cost some loss, so roaming always
		// looks tempting but rarely helps for long.
		add(1, 0, 5180, 36, "5GHz", "80MHz", "802.11ax", 20, 46.4)
		add(2, 90, 5745, 149, "5GHz", "80MHz", "802.11ax", 20, 46.4)
		w.x, w.Speed, w.shadowStep = 45, 0, 2.0
		w.cur, w.since = w.aps[0], time.Now()
		return w, nil
	}
	if scenario != "walk" && scenario != "" {
		return nil, fmt.Errorf("unknown sim scenario %q (walk, boundary)", scenario)
	}
	// Four dual-band APs along an 80 m hallway, plus a 2.4 GHz-only AP.
	chans5 := []int{36, 100, 149, 52}
	chans6 := []int{37, 85, 133, 181}
	for i, x := range []float64{0, 26, 52, 78} {
		add(i+1, x, 5000+5*chans5[i], chans5[i], "5GHz", "80MHz",
			"802.11ax", 20, 46.4)
		add(i+1, x, 5950+5*chans6[i], chans6[i], "6GHz", "160MHz",
			"802.11be", 18, 48.0)
	}
	add(9, 39, 2437, 6, "2.4GHz", "20MHz", "802.11n", 17, 40.0)
	w.cur = w.aps[0]
	w.since = time.Now()
	return w, nil
}

func (w *World) rssiOf(a *ap) float64 {
	d := math.Max(1, math.Hypot(a.x-w.x, a.y))
	return a.txPower - (a.pl1m + 30*math.Log10(d)) + a.shadow
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
	if w.Speed > 0 && now.After(w.pauseTil) {
		w.x += w.dir * w.Speed * dt
		// Turn around at the ends; sometimes stop at a "desk".
		if w.x > 82 || w.x < -4 {
			w.dir = -w.dir
		}
		if rand.Float64() < dt/40 {
			w.pauseTil = now.Add(time.Duration(15+rand.IntN(30)) * time.Second)
		}
	}
	for i, a := range w.aps {
		a.shadow += (rand.Float64()-0.5)*w.shadowStep - a.shadow*0.02
		// AP 3's radios get congested for a minute out of every three.
		target := 20.0
		if i/2 == 2 && int(now.Sub(w.start).Minutes())%3 == 1 {
			target = 88
		}
		a.util += (target-a.util)*0.05 + (rand.Float64()-0.5)*2
		a.util = math.Max(1, math.Min(99, a.util))
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

func (w *World) probe(now time.Time) (linkq.Sample, uint64, uint64) {
	if w.down || w.busy == "roam" {
		return linkq.Sample{At: now, Lost: true}, 0, 0
	}
	return w.probeAP(now, w.cur, w.busy == "scan")
}

// probeCounterfactuals samples every AP as if the client were associated to
// it and not scanning. Below the sensitivity floor it would have dropped.
func (w *World) probeCounterfactuals(now time.Time) {
	if w.cf == nil {
		w.cf = map[string]*linkq.Ring{}
		for _, a := range w.aps {
			w.cf[a.bssid] = linkq.NewRing(5 * time.Minute)
		}
	}
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
	snr := w.rssiOf(a) + rand.NormFloat64()*1.5 - noiseFloor
	util := a.util
	pLoss := 0.5/(1+math.Exp((snr-12)/2.2)) + math.Max(0, util-60)/40*0.08
	if scanning {
		pLoss += 0.45
	}
	lat := 1.5 + math.Max(0, 25-snr)*0.8 + math.Max(0, util-40)*0.25
	lat += math.Abs(rand.NormFloat64()) * (1 + math.Max(0, util-50)*0.12 +
		math.Max(0, 22-snr)*0.4)
	out := uint64(40 + rand.IntN(20))
	retx := uint64(float64(out) * math.Min(0.5, pLoss*1.5))
	if rand.Float64() < pLoss {
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
		114.7, 129, 143.4} // 802.11ax 20 MHz, 2SS, 0.8 us GI
	f := map[string]float64{"20MHz": 1, "40MHz": 2, "80MHz": 4.2,
		"160MHz": 8.4}[width]
	return int(base[min(mcs, 11)] * f * 2 * 1e6)
}

func (w *World) Link(_ context.Context) (agent.Link, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.down {
		return agent.Link{SSID: w.SSID, WPAState: "DISCONNECTED"}, nil
	}
	rssi := w.rssiOf(w.cur) + rand.NormFloat64()
	m := mcsFor(rssi - noiseFloor)
	return agent.Link{
		SSID: w.SSID, BSSID: w.cur.bssid, Freq: w.cur.freq,
		WPAState: "COMPLETED", RSSI: int(math.Round(rssi)),
		TxMCS: max(0, m-1), RxMCS: m,
		TxBitrate: rateFor(max(0, m-1), w.cur.width),
		RxBitrate: rateFor(m, w.cur.width),
		TxPHY:     "HE", RxPHY: "HE", Width: w.cur.width,
		Connected: time.Since(w.since),
	}, nil
}

func (w *World) Scan(ctx context.Context, freqs []int) error {
	n := len(freqs)
	if freqs == nil {
		n = 38 // a 2.4+5+6 GHz sweep (6 GHz PSC only)
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
	case <-time.After(time.Duration(n) * 70 * time.Millisecond):
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	want := map[int]bool{}
	for _, f := range freqs {
		want[f] = true
	}
	now := time.Now()
	for _, a := range w.aps {
		if freqs != nil && !want[a.freq] {
			continue
		}
		rssi := w.rssiOf(a) + rand.NormFloat64()*2
		if rssi < -88 {
			delete(w.cache, a.bssid)
			continue
		}
		w.cache[a.bssid] = agent.BSS{
			BSSID: a.bssid, SSID: w.SSID, Freq: a.freq, Channel: a.channel,
			Band: a.band, RSSI: int(math.Round(rssi)),
			SNR: int(math.Round(rssi)) - noiseFloor, Width: a.width,
			PHY: a.phy, UtilPct: int(a.util), Stations: 3 + int(a.util/8),
			EstThroughputKbps: rateFor(mcsFor(rssi-noiseFloor), a.width) /
				2000,
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
	w.mu.Unlock()
	d := 60 * time.Millisecond
	if !target.ft {
		d = 350 * time.Millisecond
	}
	d += time.Duration(rand.IntN(40)) * time.Millisecond
	select {
	case <-ctx.Done():
		return agent.RoamResult{}, ctx.Err()
	case <-time.After(d):
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.busy = ""
	if rssi < -82 || rand.Float64() < 0.03 {
		return agent.RoamResult{Duration: d,
			Message: "Assoc rejected - Association denied, poor channel " +
				"conditions"}, nil
	}
	w.cur, w.since = target, time.Now()
	return agent.RoamResult{Success: true, Final: bssid, Duration: d}, nil
}

func (w *World) Prepare() (func(), error) { return func() {}, nil }

// Position reports the client's x position, for the UI.
func (w *World) Position() float64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.x
}
