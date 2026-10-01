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
	"slices"
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
}

func DefaultConfig() Config {
	return Config{
		Interval:          3 * time.Second,
		JevTimeout:        1500 * time.Millisecond,
		MaxCandidates:     6,
		RoamMinConfidence: 0.5,
		MinRoamGap:        5 * time.Second,
		MinScanGap:        4 * time.Second,
		BudgetUSD:         2,
	}
}

type Agent struct {
	cfg   Config
	radio Radio
	q     *linkq.Ring
	jev   *jev.Client
	store *Store
	// Gateway reports the probed gateway for the UI, if known.
	Gateway func() string

	// Owned by the decision loop.
	ssid       string
	scan       []BSS
	scanAt     time.Time
	scanKind   string
	// scanRSSI is the current AP's RSSI when the last scan ran, so the state
	// can say how much the client's situation changed since.
	scanRSSI  int
	scanBSSID string
	lastRoam   time.Time
	connChange time.Time
	prevBSSID  string
	mem        map[string]*bssMemory
	recent     []string
	decisionID int
	selfRoam   bool
	notReady   bool
	outcomeCh  chan Outcome

	mu      sync.Mutex // guards the fields below (shared with tick loop)
	link    Link
	rssiHis []rssiPoint
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

func New(cfg Config, radio Radio, q *linkq.Ring, j *jev.Client,
	store *Store) *Agent {
	return &Agent{cfg: cfg, radio: radio, q: q, jev: j, store: store,
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
				apID(o.From), apID(o.To), o.Verdict, o.Pre.MOS, o.Post.MOS)
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
			a.mu.Lock()
			a.link = l
			if connected && l.RSSI < -1 {
				a.rssiHis = append(a.rssiHis, rssiPoint{now, l.RSSI})
				if len(a.rssiHis) > 120 {
					a.rssiHis = a.rssiHis[len(a.rssiHis)-120:]
				}
			}
			a.mu.Unlock()
			a.store.AddTick(Tick{
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
	}

	a.mu.Lock()
	paused := a.paused
	a.mu.Unlock()
	if paused {
		return
	}

	cands := a.buildCandidates(link, now)
	state := a.buildState(link, cands, now)
	qs := questions(cands)
	stateJSON, _ := json.Marshal(state)

	a.decisionID++
	d := Decision{ID: a.decisionID, T: now, Trigger: trigger,
		State: stateJSON, Questions: qs}

	a.setBusy("asking Jev")
	cctx, cancel := context.WithTimeout(ctx, a.cfg.JevTimeout)
	res, err := a.jev.Evaluate(cctx, state, qs)
	cancel()
	a.setBusy("")
	if ctx.Err() != nil {
		return // shutting down; not a Jev failure
	}

	a.mu.Lock()
	a.calls++
	if err != nil {
		a.errs++
	} else {
		a.spent += res.CostUSD
		a.latSum += float64(res.Latency.Microseconds()) / 1000
	}
	overBudget := a.cfg.BudgetUSD > 0 && a.spent >= a.cfg.BudgetUSD
	if overBudget && !a.paused {
		a.paused = true
	}
	a.mu.Unlock()
	if overBudget {
		a.store.AddNote("budget", fmt.Sprintf(
			"run budget $%.2f reached; Jev calls paused", a.cfg.BudgetUSD))
	}

	if err != nil {
		d.Err = err.Error()
		d.Executed = "nothing (Jev unavailable)"
		var apiErr *jev.APIError
		if errors.As(err, &apiErr) && apiErr.Status == 401 {
			d.Executed = "nothing (API key rejected)"
		}
		if errors.Is(err, context.DeadlineExceeded) {
			d.Err = fmt.Sprintf("timed out after %v", a.cfg.JevTimeout)
		}
		slog.Warn("Jev call failed", "err", d.Err)
		a.store.AddDecision(d)
		return
	}
	d.Answers = res.Answers
	d.Model = res.Model
	d.LatencyMs = float64(res.Latency.Microseconds()) / 1000
	d.Tokens = res.Usage.InputTokens
	d.CostUSD = res.CostUSD

	act := res.Answers["action"]
	tgt := res.Answers["target"]
	d.Chosen, d.Confidence = act.Choice, act.Confidence
	ids := candidateIDs(cands)
	if b, ok := ids[tgt.Choice]; ok {
		d.Target = b
	}
	for i := range cands {
		cands[i].Prob = tgt.Probabilities[cands[i].ID]
	}
	a.store.SetCandidates(cands)

	d.Executed, d.Blocked = a.railCheck(d, link, now)
	slog.Info("Decision", "id", d.ID, "trigger", trigger,
		"chosen", d.Chosen, "conf", d.Confidence, "target", d.Target,
		"executed", d.Executed, "blocked", d.Blocked,
		"latency_ms", int(d.LatencyMs))
	a.store.AddDecision(d)

	switch d.Executed {
	case "scan_targeted", "scan_full":
		a.doScan(ctx, d, link)
		again <- "scan_complete"
	case "roam":
		a.doRoam(ctx, d, link)
	}
}

// railCheck returns what to execute and, if that differs from Jev's choice,
// why. These are the only places code overrides Jev.
func (a *Agent) railCheck(d Decision, link Link, now time.Time) (string, string) {
	switch d.Chosen {
	case "stay", "":
		return "stay", ""
	case "scan_targeted", "scan_full":
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

func (a *Agent) doScan(ctx context.Context, d Decision, link Link) {
	kind := "full"
	var freqs []int
	if d.Executed == "scan_targeted" {
		kind = "targeted"
		for _, b := range a.scan {
			freqs = append(freqs, b.Freq)
		}
		freqs = append(freqs, link.Freq)
		slices.Sort(freqs)
		freqs = slices.Compact(freqs)
		if len(a.scan) == 0 {
			// Nothing known to target yet.
			kind, freqs = "full", nil
		}
	}
	a.setBusy(kind + " scan")
	start := time.Now()
	err := a.radio.Scan(ctx, freqs)
	dur := time.Since(start)
	act := Action{T: start, DecisionID: d.ID, Kind: "scan_" + kind,
		Freqs: freqs, DurationMs: float64(dur.Microseconds()) / 1000}
	if err == nil {
		var res []BSS
		res, err = a.radio.ScanResults(ctx, a.ssid)
		if err == nil {
			a.scan, a.scanAt, a.scanKind = res, time.Now(), kind
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
	if act.Success {
		a.addRecent("%s scan found %d APs for this SSID", kind, act.Found)
	} else {
		a.addRecent("%s scan failed", kind)
	}
}

func (a *Agent) doRoam(ctx context.Context, d Decision, link Link) {
	a.setBusy("roaming")
	start := time.Now()
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
	a.selfRoam = false
	a.addRecent("roamed %s -> %s in %d ms", apID(link.BSSID),
		apID(d.Target), r.Duration.Milliseconds())
	// Grade the roam once the post-roam window has filled.
	pre := a.q.Between(start.Add(-15*time.Second), start)
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
			RSSIBefore: link.RSSI}
		a.mu.Lock()
		o.RSSIAfter = a.link.RSSI
		a.mu.Unlock()
		o.MOSDelta = post.MOS - pre.MOS
		switch {
		case !pre.Valid || !post.Valid:
			o.Verdict = "unmeasured"
		case o.MOSDelta > 0.1:
			o.Verdict = "better"
		case o.MOSDelta < -0.1:
			o.Verdict = "worse"
		default:
			o.Verdict = "no change"
		}
		a.store.AddOutcome(o)
		select {
		case a.outcomeCh <- o:
		case <-ctx.Done():
		}
	}()
}
