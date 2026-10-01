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
	APChanges     int
	PingPongs     int // quick return (<= 60 s) where the AP left hadn't faded
	Justified     int // quick return after the AP left fell >= PingPongFallDB
	AvgStay       time.Duration
	RoamsOK       int
	RoamsFailed   int
	Better        int
	NoChange      int
	Worse         int
	AvgMOSDelta   float64
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
	var mosSum float64
	var conn, good, down int
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
	}
	if dn > 0 {
		sum.AvgMOSDelta = dsum / float64(dn)
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
	p("AP changes", "%d  (ping-pongs: %d, justified quick returns: %d, average stay %s)", s.APChanges, s.PingPongs, s.Justified, s.AvgStay.Round(time.Second))
	p("roams", "%d ok, %d failed", s.RoamsOK, s.RoamsFailed)
	p("outcomes", "%d better, %d no change, %d worse (avg ΔMOS %+.2f)", s.Better, s.NoChange, s.Worse, s.AvgMOSDelta)
	p("scans", "%d targeted, %d full (%s scanning)", s.ScansTargeted, s.ScansFull, s.ScanTime.Round(100*time.Millisecond))
	p("Jev calls", "%d  (%d errors, %d blocked by rails, median %.0f ms, $%.4f)", s.Calls, s.Errors, s.Blocked, s.MedianLatency, s.CostUSD)
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
