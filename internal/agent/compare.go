package agent

import (
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
)

// RunKey identifies a run within a paired comparison.
type RunKey struct {
	Scenario string
	Seed     uint64
	Policy   string
}

// Compare prints, per scenario, each policy's mean for the key metrics and
// the paired difference (policy B minus A) over runs that share a seed.
func Compare(w io.Writer, runs map[RunKey]Summary) {
	scen := map[string]bool{}
	pols := map[string]bool{}
	for k := range runs {
		scen[k.Scenario] = true
		pols[k.Policy] = true
	}
	ps := sortedKeys(pols)
	if len(ps) != 2 {
		_, _ = fmt.Fprintf(w, "need exactly two policies, got %v\n", ps)
		return
	}
	a, b := ps[0], ps[1] // alphabetical: classic, jev
	type metric struct {
		name   string
		get    func(Summary) float64
		higher bool // true if higher is better
		format string
	}
	metrics := []metric{
		{"avg MOS on calls", func(s Summary) float64 { return s.MOSOnCall }, true, "%.3f"},
		{"good MOS on calls (%)", func(s Summary) float64 { return s.PctGoodOnCall }, true, "%.1f"},
		{"eff Mb/s on downloads", func(s Summary) float64 { return s.EffOnDownload }, true, "%.0f"},
		{"time off channel (%)", func(s Summary) float64 { return s.PctOffChannel }, false, "%.2f"},
		{"time with good MOS (%)", func(s Summary) float64 { return s.PctGood }, true, "%.1f"},
		{"avg effective Mb/s", func(s Summary) float64 { return s.AvgEffMbps }, true, "%.0f"},
		{"average MOS", func(s Summary) float64 { return s.AvgMOS }, true, "%.3f"},
		{"avg rx PHY rate (Mb/s)", func(s Summary) float64 { return s.AvgRxMbps }, true, "%.0f"},
		{"avg RSSI (dBm)", func(s Summary) float64 { return s.AvgRSSI }, true, "%.1f"},
		{"time below -75 dBm (%)", func(s Summary) float64 { return s.PctWeak }, false, "%.1f"},
		{"time at MCS <= 3 (%)", func(s Summary) float64 { return s.PctLowMCS }, false, "%.1f"},
		{"disconnected (%)", func(s Summary) float64 { return s.PctDown }, false, "%.2f"},
		{"roams ok", func(s Summary) float64 { return float64(s.RoamsOK) }, false, "%.1f"},
		{"roams failed", func(s Summary) float64 { return float64(s.RoamsFailed) }, false, "%.2f"},
		{"ping-pongs", func(s Summary) float64 { return float64(s.PingPongs) }, false, "%.2f"},
		{"avg gain vs staying", func(s Summary) float64 { return s.AvgGainVsStay }, true, "%+.3f"},
		{"roams graded worse", func(s Summary) float64 { return float64(s.Worse) }, false, "%.2f"},
		{"safety-net full scans", func(s Summary) float64 { return float64(s.SafetyNets) }, false, "%.1f"},
		{"time scanning (s)", func(s Summary) float64 { return s.ScanTime.Seconds() }, false, "%.1f"},
	}
	for _, sc := range sortedKeys(scen) {
		var seeds []uint64
		for k := range runs {
			if k.Scenario == sc && k.Policy == a {
				if _, ok := runs[RunKey{sc, k.Seed, b}]; ok {
					seeds = append(seeds, k.Seed)
				}
			}
		}
		slices.Sort(seeds)
		_, _ = fmt.Fprintf(w, "\n== %s: %d paired runs (seeds %v)\n", sc, len(seeds), seeds)
		_, _ = fmt.Fprintf(w, "  %-24s %10s %10s %12s %14s\n", "metric", a, b, b+" - "+a, b+" better in")
		for _, m := range metrics {
			var va, vb, d []float64
			wins, losses := 0, 0
			for _, sd := range seeds {
				x, y := m.get(runs[RunKey{sc, sd, a}]), m.get(runs[RunKey{sc, sd, b}])
				va, vb, d = append(va, x), append(vb, y), append(d, y-x)
				better := y > x
				if !m.higher {
					better = y < x
				}
				switch {
				case math.Abs(y-x) < 1e-9:
				case better:
					wins++
				default:
					losses++
				}
			}
			mean, sd := meanSD(d)
			ci := ""
			if len(d) > 1 {
				// ~95% interval on the mean paired difference (t ≈ 2.26 for n=10).
				half := 2.26 * sd / math.Sqrt(float64(len(d)))
				ci = fmt.Sprintf(" ±"+strings.Replace(m.format, "%+", "%", 1), half)
			}
			ma, _ := meanSD(va)
			mb, _ := meanSD(vb)
			_, _ = fmt.Fprintf(w, "  %-24s %10s %10s %12s %14s\n", m.name,
				fmt.Sprintf(m.format, ma), fmt.Sprintf(m.format, mb),
				fmt.Sprintf(m.format, mean)+ci,
				fmt.Sprintf("%d of %d", wins, wins+losses))
		}
	}
}

func meanSD(xs []float64) (float64, float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	m := s / float64(len(xs))
	var v float64
	for _, x := range xs {
		v += (x - m) * (x - m)
	}
	if len(xs) > 1 {
		v /= float64(len(xs) - 1)
	}
	return m, math.Sqrt(v)
}

func sortedKeys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
