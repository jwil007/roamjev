// Package agent is the decision loop: observe the link, describe it to Jev,
// and carry out whatever Jev decides. Jev chooses when to scan, whether to
// roam, and where. Code only measures, describes, executes, and applies a few
// visible safety rails.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/jwil007/roamjev/internal/jev"
	"github.com/jwil007/roamjev/internal/linkq"
)

type Config struct {
	// Interval between decisions. Jev is also asked right after each scan.
	Interval time.Duration
	// JevTimeout bounds each call. A stale decision is worse than none.
	JevTimeout time.Duration
	// MaxCandidates is how many APs (besides the current one) Jev sees.
	MaxCandidates int
	// Rails. Blocked decisions are recorded with the reason.
	RoamMinConfidence float64
	MinRoamGap        time.Duration
	MinScanGap        time.Duration
	// BudgetUSD pauses Jev calls once this run has spent this much.
	BudgetUSD float64
	// Observe asks Jev but never acts on the answer.
	Observe bool
	// HideHistory leaves out the roam-count / dwell / ping-pong facts, as
	// the control arm of an A/B comparison.
	HideHistory bool
	// VerifyRoam re-measures the target's channel right before every roam
	// (one channel, ~35-105 ms). If the target is gone, the roam is
	// cancelled; if its signal moved by VerifyDeltaDB or more from what the
	// decider saw, the roam is cancelled and the decider is asked again
	// with the fresh reading. It never decides on its own to stay.
	VerifyRoam    bool
	VerifyDeltaDB int
}

func DefaultConfig() Config {
	return Config{
		Interval:          3 * time.Second,
		JevTimeout:        1500 * time.Millisecond,
		MaxCandidates:     6,
		// 0: act on Jev's top choice. Confidence measures how concentrated
		// the whole distribution is, so with five actions a clear 55%
		// "roam" scores ~0.3-0.45; a 0.5 rail blocked ~80% of Jev's roam
		// choices in the office and convention simulations.
		RoamMinConfidence: 0,
		MinRoamGap:        5 * time.Second,
		MinScanGap:        4 * time.Second,
		VerifyDeltaDB:     6,
		BudgetUSD:         2,
	}
}

type Agent struct {
	cfg    Config
	radio  Radio
	q      *linkq.Ring
	policy Policy
	store  *Store
	// Gateway reports the probed gateway for the UI, if known.
	Gateway func() string

	// Owned by the decision loop.
	ssid       string
	scan       []BSS
	scanAt     time.Time
	scanKind   string
	// scanFetched is when a.scan was last read from the radio; each BSS's
	// Age is relative to that moment.
	scanFetched time.Time
	// scanRSSI is the current AP's RSSI when the last scan ran, so the state
	// can say how much the client's situation changed since.
	scanRSSI  int
	scanBSSID string
	lastRoam   time.Time
	connChange time.Time
	prevBSSID  string
	visits     []visit
	// Measured costs (decision loop only).
	dwellActive, dwellPassive, fullScanMs float64
	roamDurs                              []int
	mem        map[string]*bssMemory
	recent     []string
	decisionID int
	selfRoam   bool
	notReady   bool
	outcomeCh  chan Outcome

	mu       sync.Mutex // guards the fields below (shared with tick loop)
	link     Link
	rssiHis  []rssiPoint
	counters []counterPoint
	busy    string
	calls   int
	errs    int
	spent   float64
	latSum  float64
	paused  bool
}

type rssiPoint struct {
	t    time.Time
	rssi int
}

func New(cfg Config, radio Radio, q *linkq.Ring, policy Policy,
	store *Store) *Agent {
	return &Agent{cfg: cfg, radio: radio, q: q, policy: policy, store: store,
		mem: map[string]*bssMemory{}, outcomeCh: make(chan Outcome, 16)}
}

// rssiAt returns the RSSI reading at or before t, ignoring readings from
// before notBefore (the last connection change).
func (a *Agent) rssiAt(t, notBefore time.Time) (int, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := len(a.rssiHis) - 1; i >= 0; i-- {
		p := a.rssiHis[i]
		if p.t.Before(notBefore) {
			return 0, false
		}
		if !p.t.After(t) {
			return p.rssi, true
		}
	}
	return 0, false
}

func (a *Agent) setBusy(s string) {
	a.mu.Lock()
	a.busy = s
	a.mu.Unlock()
	a.pushStatus()
}

func (a *Agent) addRecent(format string, args ...any) {
	line := time.Now().Format("15:04:05") + " " + fmt.Sprintf(format, args...)
	a.recent = append(a.recent, line)
	if len(a.recent) > 6 {
		a.recent = a.recent[len(a.recent)-6:]
	}
}

func (a *Agent) pushStatus() {
	a.mu.Lock()
	st := Status{
		SSID: a.link.SSID, BSSID: a.link.BSSID, Freq: a.link.Freq,
		WPAState: a.link.WPAState, Busy: a.busy, Calls: a.calls,
		Errors: a.errs, SpentUSD: a.spent, Paused: a.paused,
	}
	if a.calls > 0 {
		st.AvgLatency = a.latSum / float64(a.calls)
	}
	a.mu.Unlock()
	st.Band, st.Channel = bandChannel(st.Freq)
	if a.Gateway != nil {
		st.Gateway = a.Gateway()
	}
	a.store.SetStatus(st)
}

func bandChannel(freq int) (string, int) {
	switch {
	case freq == 2484:
		return "2.4GHz", 14
	case freq >= 2412 && freq < 2484:
		return "2.4GHz", (freq - 2407) / 5
	case freq >= 5955 && freq <= 7115:
		return "6GHz", (freq - 5950) / 5
	case freq >= 5160 && freq <= 5885:
		return "5GHz", (freq - 5000) / 5
	}
	return "", 0
}

// Run blocks until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	restore, err := a.radio.Prepare()
	if err != nil {
		return err
	}
	defer restore()

	go a.tickLoop(ctx)

	trigger := make(chan string, 1)
	t := time.NewTicker(a.cfg.Interval)
	defer t.Stop()
	a.decide(ctx, "startup", trigger)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			a.decide(ctx, "interval", trigger)
		case why := <-trigger:
			a.decide(ctx, why, trigger)
			t.Reset(a.cfg.Interval)
		case o := <-a.outcomeCh:
			m := a.mem[o.To]
			if m == nil {
				m = &bssMemory{}
				a.mem[o.To] = m
			}
			m.lastOutcome = &o
			a.addRecent("roam %s -> %s graded %s: MOS %.2f -> %.2f",
				apID(o.From), apID(o.To), o.VerdictBA, o.Pre.MOS, o.Post.MOS)
		}
	}
}

// tickLoop records a timeline sample every second, independent of decisions
// (which block during scans and roams).
func (a *Agent) tickLoop(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			l, err := a.radio.Link(ctx)
			if err != nil {
				continue
			}
			w := a.q.Last(3 * time.Second)
			connected := l.WPAState == "COMPLETED" && l.BSSID != ""
			rx, tx, cerr := a.radio.Counters()
			a.mu.Lock()
			if cerr == nil {
				a.counters = append(a.counters, counterPoint{now, rx, tx})
				if len(a.counters) > 120 {
					a.counters = a.counters[len(a.counters)-120:]
				}
			}
			a.link = l
			if connected && l.RSSI < -1 {
				a.rssiHis = append(a.rssiHis, rssiPoint{now, l.RSSI})
				if len(a.rssiHis) > 120 {
					a.rssiHis = a.rssiHis[len(a.rssiHis)-120:]
				}
			}
			a.mu.Unlock()
			kbps, _ := a.trafficKbps(now, 3*time.Second)
			activity := ""
			if ar, ok := a.radio.(ActivityReporter); ok {
				activity = ar.Activity()
			}
			a.store.AddTick(Tick{
				UtilPct:     l.UtilPct,
				EffMbps:     effMbps(l.RxBitrate, l.UtilPct),
				TrafficKbps: math.Round(kbps),
				Activity:    activity,
				T: now, BSSID: l.BSSID, Freq: l.Freq, RSSI: l.RSSI,
				TxMCS: l.TxMCS, RxMCS: l.RxMCS,
				TxMbps:    float64(l.TxBitrate) / 1e6,
				RxMbps:    float64(l.RxBitrate) / 1e6,
				MOS:       w.MOS, LossPct: w.LossPct,
				LatencyMs: w.LatencyMs, JitterMs: w.JitterMs,
				TCPRetransPct: a.q.Last(10 * time.Second).TCPRetransPct,
				TCPOutSegs:    a.q.Last(10 * time.Second).TCPOutSegs,
				Connected:     connected,
			})
			a.pushStatus()
		}
	}
}

func (a *Agent) decide(ctx context.Context, trigger string,
	again chan<- string) {
	now := time.Now()
	link, err := a.radio.Link(ctx)
	if err != nil {
		slog.Warn("Reading link failed", "err", err)
		return
	}
	if link.WPAState != "COMPLETED" || link.BSSID == "" {
		if !a.notReady {
			a.store.AddNote("link", "not connected (wpa_state "+
				link.WPAState+"); decisions paused")
			a.notReady = true
		}
		return
	}
	if a.notReady {
		a.store.AddNote("link", "connected again to "+link.BSSID)
		a.notReady = false
	}
	if link.SSID != a.ssid {
		if a.ssid != "" {
			a.store.AddNote("link", "SSID changed to "+link.SSID)
			// Re-apply roaming settings to the new network block.
			if _, err := a.radio.Prepare(); err != nil {
				slog.Error("Prepare after SSID change", "err", err)
			}
			a.scan, a.scanAt = nil, time.Time{}
		}
		a.ssid = link.SSID
	}
	if link.BSSID != a.prevBSSID {
		if a.prevBSSID != "" && !a.selfRoam {
			a.store.AddNote("external", fmt.Sprintf(
				"BSSID changed outside the agent: %s -> %s",
				a.prevBSSID, link.BSSID))
			a.addRecent("connection moved to %s without a decision "+
				"(external)", apID(link.BSSID))
		}
		a.connChange = now
		if link.Connected > 0 && link.Connected < time.Hour {
			a.connChange = now.Add(-link.Connected)
		}
		a.prevBSSID = link.BSSID
		a.selfRoam = false
		a.recordJoin(link.BSSID, a.connChange)
		a.policy.Notify(PolicyEvent{Kind: "conn_change", At: a.connChange,
			Target: link.BSSID})
	}
	a.observeRSSI(link.BSSID, link.RSSI)

	a.mu.Lock()
	paused := a.paused
	a.mu.Unlock()
	if paused {
		return
	}

	if a.cfg.Observe {
		a.refreshPassive(ctx, link)
	}
	cands := a.buildCandidates(link, now)
	state := a.buildState(link, cands, now)
	stateJSON, _ := json.Marshal(state)

	a.decisionID++
	d := Decision{ID: a.decisionID, T: now, Trigger: trigger,
		State: stateJSON, Policy: a.policy.Name()}

	if _, isJev := a.policy.(*JevPolicy); isJev {
		a.setBusy("asking Jev")
	}
	out, err := a.policy.Decide(ctx, PolicyInput{Now: now, Link: link,
		Cands: cands, Scan: a.scan, ScanAt: a.scanAt, State: state,
		LastRoam: a.lastRoam, ConnChange: a.connChange})
	a.setBusy("")
	if ctx.Err() != nil {
		return // shutting down; not a decider failure
	}
	d.Questions = out.Questions
	d.Reason = out.Reason

	if out.UsedJev {
		a.mu.Lock()
		a.calls++
		if err != nil {
			a.errs++
		} else {
			a.spent += out.CostUSD
			a.latSum += float64(out.Latency.Microseconds()) / 1000
		}
		overBudget := a.cfg.BudgetUSD > 0 && a.spent >= a.cfg.BudgetUSD
		newlyPaused := overBudget && !a.paused
		if newlyPaused {
			a.paused = true
		}
		a.mu.Unlock()
		if newlyPaused {
			a.store.AddNote("budget", fmt.Sprintf(
				"run budget $%.2f reached; Jev calls paused", a.cfg.BudgetUSD))
		}
	}

	if err != nil {
		d.Err = err.Error()
		d.Executed = "nothing (decider unavailable)"
		var apiErr *jev.APIError
		if errors.As(err, &apiErr) && apiErr.Status == 401 {
			d.Executed = "nothing (API key rejected)"
		}
		if errors.Is(err, context.DeadlineExceeded) {
			d.Err = fmt.Sprintf("timed out after %v", a.cfg.JevTimeout)
		}
		slog.Warn("Decision failed", "err", d.Err)
		a.store.AddDecision(d)
		return
	}
	d.Answers = out.Answers
	d.Model = out.Model
	d.LatencyMs = float64(out.Latency.Microseconds()) / 1000
	d.Tokens = out.Tokens
	d.CostUSD = out.CostUSD
	d.Chosen, d.Confidence, d.Target = out.Chosen, out.Confidence, out.Target
	if d.Chosen == "scan_targeted" {
		d.Chosen = "scan_known" // older name, still used by the classic policy
	}
	if tgt, ok := out.Answers["target"]; ok {
		for i := range cands {
			cands[i].Prob = tgt.Probabilities[cands[i].ID]
		}
	}
	a.store.SetCandidates(cands)

	d.Executed, d.Blocked = a.railCheck(d, link, now)
	slog.Info("Decision", "id", d.ID, "trigger", trigger,
		"chosen", d.Chosen, "conf", d.Confidence, "target", d.Target,
		"executed", d.Executed, "blocked", d.Blocked,
		"latency_ms", int(d.LatencyMs), "reason", d.Reason)
	a.store.AddDecision(d)

	switch d.Executed {
	case "scan_quick", "scan_known", "scan_full":
		a.doScan(ctx, d, link, cands)
		again <- "scan_complete"
	case "roam":
		if a.cfg.VerifyRoam {
			if why := a.verifyTarget(ctx, d, link); why != "" {
				again <- why
				return
			}
		}
		a.doRoam(ctx, d, link)
	}
}

// railCheck returns what to execute and, if that differs from Jev's choice,
// why. These are the only places code overrides Jev.
func (a *Agent) railCheck(d Decision, link Link, now time.Time) (string, string) {
	switch d.Chosen {
	case "stay", "":
		return "stay", ""
	case "scan_quick", "scan_known", "scan_full":
		if a.cfg.Observe {
			return "stay", "observe mode"
		}
		if since := now.Sub(a.scanAt); !a.scanAt.IsZero() &&
			since < a.cfg.MinScanGap {
			return "stay", fmt.Sprintf("scan rail: last scan %s ago (min %s)",
				since.Round(100*time.Millisecond), a.cfg.MinScanGap)
		}
		return d.Chosen, ""
	case "roam":
		switch {
		case d.Target == "":
			return "stay", "roam chosen but target was \"none\""
		case d.Target == link.BSSID:
			return "stay", "roam target is the current AP"
		case d.Confidence < a.cfg.RoamMinConfidence:
			return "stay", fmt.Sprintf("confidence rail: %.2f < %.2f",
				d.Confidence, a.cfg.RoamMinConfidence)
		case !a.lastRoam.IsZero() && now.Sub(a.lastRoam) < a.cfg.MinRoamGap:
			return "stay", fmt.Sprintf("roam rail: last roam %s ago (min %s)",
				now.Sub(a.lastRoam).Round(100*time.Millisecond),
				a.cfg.MinRoamGap)
		case a.cfg.Observe:
			return "stay", "observe mode"
		}
		return "roam", ""
	}
	return "stay", "unknown action " + d.Chosen
}

func (a *Agent) doScan(ctx context.Context, d Decision, link Link,
	cands []Candidate) {
	quick, known := a.scanScopes(link, cands)
	kind := strings.TrimPrefix(d.Executed, "scan_")
	var freqs []int
	switch kind {
	case "quick":
		freqs = quick
	case "known":
		freqs = known
	}
	if kind != "full" && (len(a.scan) == 0 || len(freqs) == 0) {
		kind, freqs = "full", nil // nothing known to target yet
	}
	a.setBusy(kind + " scan")
	start := time.Now()
	err := a.radio.Scan(ctx, freqs)
	dur := time.Since(start)
	act := Action{T: start, DecisionID: d.ID, Kind: "scan_" + kind,
		Freqs: freqs, DurationMs: float64(dur.Microseconds()) / 1000}
	if err == nil {
		a.learnScanCost(freqs, dur)
		var res []BSS
		res, err = a.radio.ScanResults(ctx, a.ssid)
		if err == nil {
			a.scan, a.scanAt, a.scanKind = res, time.Now(), kind
			a.scanFetched = a.scanAt
			a.scanRSSI, a.scanBSSID = link.RSSI, link.BSSID
			act.Found = len(res)
			act.Success = true
		}
	}
	if err != nil {
		act.Message = err.Error()
		slog.Warn("Scan failed", "err", err)
	}
	a.setBusy("")
	a.store.AddAction(act)
	a.policy.Notify(PolicyEvent{Kind: "scan", At: time.Now(),
		Success: act.Success, Message: kind})
	if act.Success {
		a.addRecent("%s scan (%d channels, %d ms) found %d APs for this SSID",
			kind, max(len(freqs), 0), dur.Milliseconds(), act.Found)
	} else {
		a.addRecent("%s scan failed", kind)
	}
}

func (a *Agent) doRoam(ctx context.Context, d Decision, link Link) {
	a.setBusy("roaming")
	start := time.Now()
	// Joined-time of the AP we're leaving; the baseline must not reach back
	// past it. (a.connChange is overwritten below on success.)
	prevConn := a.connChange
	a.selfRoam = true
	r, err := a.radio.Roam(ctx, d.Target)
	done := time.Now()
	a.setBusy("")
	a.lastRoam = done
	act := Action{T: start, DecisionID: d.ID, Kind: "roam",
		From: link.BSSID, Target: d.Target,
		DurationMs: float64(r.Duration.Microseconds()) / 1000,
		Success:    err == nil && r.Success, Message: r.Message}
	if err != nil {
		act.Message = err.Error()
	}
	a.store.AddAction(act)
	a.policy.Notify(PolicyEvent{Kind: "roam", At: done, Target: d.Target,
		Success: act.Success, Message: act.Message})
	m := a.mem[d.Target]
	if m == nil {
		m = &bssMemory{}
		a.mem[d.Target] = m
	}
	if !act.Success {
		a.selfRoam = false
		m.failures++
		m.lastFailAt = done
		m.lastFailMsg = act.Message
		a.addRecent("roam %s -> %s FAILED: %s", apID(link.BSSID),
			apID(d.Target), act.Message)
		return
	}
	a.connChange = done
	a.prevBSSID = d.Target
	a.recordJoin(d.Target, done)
	a.roamDurs = append(a.roamDurs, int(r.Duration.Milliseconds()))
	if len(a.roamDurs) > 7 {
		a.roamDurs = a.roamDurs[1:]
	}
	a.selfRoam = false
	a.addRecent("roamed %s -> %s in %d ms", apID(link.BSSID),
		apID(d.Target), r.Duration.Milliseconds())
	// Grade the roam once the post-roam window has filled.
	pre := a.q.Between(start.Add(-15*time.Second), start)
	baseFrom := start.Add(-75 * time.Second)
	if prevConn.After(baseFrom) {
		baseFrom = prevConn
	}
	baseline := a.q.Between(baseFrom, start.Add(-15*time.Second))
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(18 * time.Second):
		}
		post := a.q.Between(done.Add(3*time.Second), done.Add(18*time.Second))
		o := Outcome{T: time.Now(), DecisionID: d.ID, From: link.BSSID,
			To: d.Target, Pre: pre, Post: post,
			Disruption: a.q.Between(start, done.Add(3*time.Second)),
			Baseline:   baseline,
			Actual:     a.q.Between(start, done.Add(18*time.Second)),
			RSSIBefore: link.RSSI}
		a.mu.Lock()
		o.RSSIAfter = a.link.RSSI
		a.mu.Unlock()
		o.MOSDelta = post.MOS - pre.MOS
		o.VerdictBA = grade(o.MOSDelta, pre.Valid && post.Valid)
		o.Verdict, o.Basis = o.VerdictBA, "before/after"
		if cf, ok := a.radio.(Counterfactual); ok {
			// Same window as Actual, so the roam's own disruption counts.
			if stay, ok := cf.StayQuality(link.BSSID, start,
				done.Add(18*time.Second)); ok {
				o.Stay = &stay
				o.GainVsStay = o.Actual.MOS - stay.MOS
				o.Verdict = grade(o.GainVsStay, o.Actual.Valid)
				o.Basis = "vs stay"
			}
		}
		a.store.AddOutcome(o)
		select {
		case a.outcomeCh <- o:
		case <-ctx.Done():
		}
	}()
}

func grade(delta float64, valid bool) string {
	switch {
	case !valid:
		return "unmeasured"
	case delta > 0.1:
		return "better"
	case delta < -0.1:
		return "worse"
	}
	return "no change"
}

// refreshPassive reads wpa_supplicant's scan cache without scanning. In
// observe mode roamjev never scans, so this is how Jev sees candidates:
// whatever wpa_supplicant (or roamctl) scanned recently. BSS ages come from
// the cache, and the current AP's cached RSSI anchors the "since scan" fact.
func (a *Agent) refreshPassive(ctx context.Context, link Link) {
	res, err := a.radio.ScanResults(ctx, a.ssid)
	if err != nil || len(res) == 0 {
		return
	}
	a.scan, a.scanAt, a.scanKind = res, time.Now(), "cached (passive)"
	a.scanFetched = a.scanAt
	a.scanBSSID = ""
	for _, b := range res {
		if b.BSSID == link.BSSID {
			a.scanRSSI, a.scanBSSID = b.RSSI, b.BSSID
		}
	}
	if a.scanBSSID == "" {
		// Current AP not in the cache: say nothing rather than mislead.
		a.scanRSSI, a.scanBSSID = link.RSSI, link.BSSID
	}
}

// verifyTarget scans just the target's channel before a roam. It returns a
// re-decide trigger when the roam should not go ahead as decided, or "" to
// proceed. Scan readings carry a few dB of noise, so only a change of
// VerifyDeltaDB or more counts as the data having moved.
func (a *Agent) verifyTarget(ctx context.Context, d Decision, link Link) string {
	seen, freq := 0, 0
	for _, b := range a.scan {
		if b.BSSID == d.Target {
			seen, freq = b.RSSI, b.Freq
		}
	}
	if freq == 0 {
		return "" // nothing to verify against; let the roam proceed
	}
	a.setBusy("checking target")
	start := time.Now()
	err := a.radio.Scan(ctx, []int{freq})
	dur := time.Since(start)
	act := Action{T: start, DecisionID: d.ID, Kind: "scan_verify",
		Freqs: []int{freq}, Target: d.Target,
		DurationMs: float64(dur.Microseconds()) / 1000}
	var res []BSS
	if err == nil {
		res, err = a.radio.ScanResults(ctx, a.ssid)
	}
	a.setBusy("")
	if err != nil {
		act.Message = err.Error()
		a.store.AddAction(act)
		return "" // couldn't check; don't block the decision on it
	}
	a.learnScanCost([]int{freq}, dur)
	act.Success = true
	a.scan, a.scanFetched = res, time.Now()
	now := -999
	for _, b := range res {
		if b.BSSID == d.Target && b.Age < 2*time.Second {
			now = b.RSSI
		}
	}
	switch {
	case now == -999:
		act.Message = "target not heard; roam cancelled"
		a.addRecent("pre-roam check: %s not heard on its channel; roam "+
			"cancelled", apID(d.Target))
		a.store.AddAction(act)
		return "verify_gone"
	case now-seen >= a.cfg.VerifyDeltaDB || seen-now >= a.cfg.VerifyDeltaDB:
		act.Message = fmt.Sprintf("target now %d dBm (decided on %d); "+
			"asking again", now, seen)
		a.addRecent("pre-roam check: %s re-measured at %d dBm (was %d); "+
			"roam re-evaluated", apID(d.Target), now, seen)
		a.store.AddAction(act)
		return "verify_changed"
	}
	act.Message = fmt.Sprintf("target %d dBm (decided on %d); roaming", now, seen)
	a.store.AddAction(act)
	return ""
}
