package agent

import (
	"fmt"
	"io"
	"slices"
	"time"
)

// Summary is a run's scorecard, computed from a journal. The dashboard
// computes the same numbers in JS; this is for comparing runs side by side.
type Summary struct {
	Mode          string
	Duration      time.Duration
	AvgMOS        float64
	PctGood       float64 // share of connected seconds with MOS >= 4.03
	PctDown       float64
	AvgRxMbps     float64 // PHY rate: what the link could carry
	AvgEffMbps    float64 // PHY rate x free airtime
	// Judged when each matters (simulator ground truth for the activity).
	PctGoodOnCall  float64 // share of call seconds with MOS >= 4.03
	MOSOnCall      float64 // average MOS during calls
	EffOnDownload  float64 // avg effective Mb/s during downloads
	PctOffChannel  float64 // share of the run spent scanning or roaming
	AvgRoamMs      float64
	AvgRSSI       float64
	PctWeak       float64 // share of connected seconds below -75 dBm
	PctLowMCS     float64 // share of connected seconds at rx MCS <= 3
	APChanges     int
	PingPongs     int // quick return (<= 60 s) where the AP left hadn't faded
	Justified     int // quick return after the AP left fell >= PingPongFallDB
	AvgStay       time.Duration
	RoamsOK       int
	RoamsFailed   int
	Better        int
	NoChange      int
	Worse         int
	AvgMOSDelta   float64 // before/after (optimistic)
	AvgGainVsStay float64 // simulator only
	StayGraded    int
	ScansTargeted int
	ScansFull     int
	ScanTime      time.Duration
	Calls         int
	Errors        int
	Blocked       int
	MedianLatency float64
	CostUSD       float64
	// Mean yes-probability of "roam would be short-lived" on decisions where
	// Jev chose to roam vs chose not to.
	ShortLivedWhenRoam float64
	ShortLivedWhenNot  float64
}

func Summarize(s *Store) Summary {
	snap := s.Snapshot()
	var sum Summary
	sum.Mode = snap.Info.Mode
	if n := len(snap.Ticks); n > 1 {
		sum.Duration = snap.Ticks[n-1].T.Sub(snap.Ticks[0].T)
	}
	var mosSum, rateSum, rssiSum, effSum, dlEff, callMOS float64
	var conn, good, down, linked, weak, lowMCS, calls, callGood, dls int
	type change struct {
		t        time.Time
		from, to string
		fall     int // how much the AP being left fell during the stay
	}
	var changes []change
	var stays []time.Duration
	prev := ""
	var joined time.Time
	var stayRSSI []int
	for _, t := range snap.Ticks {
		if !t.Connected || t.BSSID == "" {
			down++
			continue
		}
		if prev != "" && t.BSSID != prev {
			changes = append(changes, change{t.T, prev, t.BSSID, stayFall(stayRSSI)})
			stayRSSI = nil
			// The first AP's start is unknown, so its stay isn't counted.
			if !joined.IsZero() {
				stays = append(stays, t.T.Sub(joined))
			}
			joined = t.T
		}
		stayRSSI = append(stayRSSI, t.RSSI)
		linked++
		rateSum += t.RxMbps
		effSum += t.EffMbps
		switch t.Activity {
		case "call":
			calls++
			callMOS += t.MOS
			if t.MOS >= 4.03 {
				callGood++
			}
		case "download":
			dls++
			dlEff += t.EffMbps
		}
		rssiSum += float64(t.RSSI)
		if t.RSSI < -75 {
			weak++
		}
		if t.RxMCS <= 3 {
			lowMCS++
		}
		if t.MOS > 0 {
			conn++
			mosSum += t.MOS
			if t.MOS >= 4.03 {
				good++
			}
		}
		prev = t.BSSID
	}
	if conn > 0 {
		sum.AvgMOS = mosSum / float64(conn)
		sum.PctGood = 100 * float64(good) / float64(conn)
	}
	if n := len(snap.Ticks); n > 0 {
		sum.PctDown = 100 * float64(down) / float64(n)
	}
	if linked > 0 {
		sum.AvgRxMbps = rateSum / float64(linked)
		sum.AvgRSSI = rssiSum / float64(linked)
		sum.PctWeak = 100 * float64(weak) / float64(linked)
		sum.PctLowMCS = 100 * float64(lowMCS) / float64(linked)
		sum.AvgEffMbps = effSum / float64(linked)
	}
	if calls > 0 {
		sum.PctGoodOnCall = 100 * float64(callGood) / float64(calls)
		sum.MOSOnCall = callMOS / float64(calls)
	}
	if dls > 0 {
		sum.EffOnDownload = dlEff / float64(dls)
	}
	sum.APChanges = len(changes)
	for i := 1; i < len(changes); i++ {
		a, b := changes[i-1], changes[i]
		if b.to == a.from && b.from == a.to && b.t.Sub(a.t) <= time.Minute {
			if b.fall >= PingPongFallDB {
				sum.Justified++
			} else {
				sum.PingPongs++
			}
		}
	}
	if len(stays) > 0 {
		var tot time.Duration
		for _, d := range stays {
			tot += d
		}
		sum.AvgStay = tot / time.Duration(len(stays))
	}
	for _, a := range snap.Actions {
		switch a.Kind {
		case "roam":
			if a.Success {
				sum.RoamsOK++
			} else {
				sum.RoamsFailed++
			}
		case "scan_targeted":
			sum.ScansTargeted++
		case "scan_full":
			sum.ScansFull++
		}
		if a.Kind != "roam" {
			sum.ScanTime += time.Duration(a.DurationMs * float64(time.Millisecond))
		}
	}
	var roamMs float64
	var nr int
	for _, a := range snap.Actions {
		if a.Kind == "roam" {
			roamMs += a.DurationMs
			nr++
		}
	}
	if nr > 0 {
		sum.AvgRoamMs = roamMs / float64(nr)
	}
	if sum.Duration > 0 {
		sum.PctOffChannel = 100 * (sum.ScanTime.Seconds() + roamMs/1000) /
			sum.Duration.Seconds()
	}
	var dsum float64
	var dn int
	for _, o := range snap.Outcomes {
		switch o.Verdict {
		case "better":
			sum.Better++
		case "worse":
			sum.Worse++
		case "no change":
			sum.NoChange++
		}
		if o.Verdict != "unmeasured" {
			dsum += o.MOSDelta
			dn++
		}
		if o.Stay != nil {
			sum.AvgGainVsStay += o.GainVsStay
			sum.StayGraded++
		}
	}
	if dn > 0 {
		sum.AvgMOSDelta = dsum / float64(dn)
	}
	if sum.StayGraded > 0 {
		sum.AvgGainVsStay /= float64(sum.StayGraded)
	}
	var lats []float64
	var slR, slN float64
	var nR, nN int
	for _, d := range snap.Decisions {
		sum.Calls++
		sum.CostUSD += d.CostUSD
		if d.Err != "" {
			sum.Errors++
			continue
		}
		if d.Blocked != "" {
			sum.Blocked++
		}
		lats = append(lats, d.LatencyMs)
		if a, ok := d.Answers["roam_short_lived"]; ok {
			if d.Chosen == "roam" {
				slR += a.Noul
				nR++
			} else {
				slN += a.Noul
				nN++
			}
		}
	}
	if len(lats) > 0 {
		slices.Sort(lats)
		sum.MedianLatency = lats[len(lats)/2]
	}
	if nR > 0 {
		sum.ShortLivedWhenRoam = slR / float64(nR)
	}
	if nN > 0 {
		sum.ShortLivedWhenNot = slN / float64(nN)
	}
	return sum
}

func (s Summary) Print(w io.Writer, name string) {
	p := func(k, f string, v ...any) { _, _ = fmt.Fprintf(w, "  %-22s "+f+"\n", append([]any{k}, v...)...) }
	_, _ = fmt.Fprintf(w, "%s  [%s]\n", name, s.Mode)
	p("duration", "%s", s.Duration.Round(time.Second))
	p("average MOS", "%.2f  (%.0f%% of time good, %.1f%% disconnected)", s.AvgMOS, s.PctGood, s.PctDown)
	p("by activity", "calls: avg MOS %.2f, good %.1f%% of the time; downloads: %.0f Mb/s effective", s.MOSOnCall, s.PctGoodOnCall, s.EffOnDownload)
	p("off channel", "%.2f%% of the run scanning or roaming (avg roam %.0f ms)", s.PctOffChannel, s.AvgRoamMs)
	p("signal and rate", "avg RSSI %.1f dBm, avg rx PHY rate %.0f Mb/s, %.1f%% of time below -75 dBm, %.1f%% at MCS <= 3", s.AvgRSSI, s.AvgRxMbps, s.PctWeak, s.PctLowMCS)
	p("AP changes", "%d  (ping-pongs: %d, justified quick returns: %d, average stay %s)", s.APChanges, s.PingPongs, s.Justified, s.AvgStay.Round(time.Second))
	p("roams", "%d ok, %d failed", s.RoamsOK, s.RoamsFailed)
	basis := "before/after"
	if s.StayGraded > 0 {
		basis = fmt.Sprintf("vs staying; avg gain %+.2f", s.AvgGainVsStay)
	}
	p("outcomes", "%d better, %d no change, %d worse (%s; before/after avg %+.2f)", s.Better, s.NoChange, s.Worse, basis, s.AvgMOSDelta)
	p("scans", "%d targeted, %d full (%s scanning)", s.ScansTargeted, s.ScansFull, s.ScanTime.Round(100*time.Millisecond))
	if s.CostUSD > 0 {
		p("Jev calls", "%d  (%d errors, %d blocked by rails, median %.0f ms, $%.4f)", s.Calls, s.Errors, s.Blocked, s.MedianLatency, s.CostUSD)
	} else {
		p("decisions", "%d  (%d blocked by rails)", s.Calls, s.Blocked)
	}
	p("short-lived? (noul)", "%.2f when choosing roam, %.2f otherwise", s.ShortLivedWhenRoam, s.ShortLivedWhenNot)
}

// stayFall is how far RSSI fell over a stay: the median of the first three
// readings minus the median of the last three (per-second ticks are noisy).
func stayFall(r []int) int {
	if len(r) < 2 {
		return 0
	}
	med := func(xs []int) int {
		c := slices.Clone(xs)
		slices.Sort(c)
		return c[len(c)/2]
	}
	k := min(3, len(r)/2)
	return med(r[:k]) - med(r[len(r)-k:])
}
