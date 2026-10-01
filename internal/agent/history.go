package agent

import (
	"fmt"
	"strings"
	"time"
)

// visit is one stint on an AP. left is zero while it is the current AP.
type visit struct {
	bssid  string
	joined time.Time
	left   time.Time
	// RSSI at the first and last observation during the stay, so a return
	// to the previous AP can be told apart from noise-driven ping-pong: if
	// the AP being left really faded, the client was probably moving.
	firstRSSI, lastRSSI int
	hasRSSI             bool
}

// fall is how much the signal dropped during the stay (positive = fell).
func (v visit) fall() int { return v.firstRSSI - v.lastRSSI }

// PingPongFallDB: a quick return only counts as ping-pong if the AP being
// left fell less than this during the stay. Used for grading, never for
// decisions.
const PingPongFallDB = 6

// observeRSSI updates the open visit with a reading on the current AP.
func (a *Agent) observeRSSI(bssid string, rssi int) {
	n := len(a.visits)
	if n == 0 || a.visits[n-1].bssid != bssid || !a.visits[n-1].left.IsZero() ||
		rssi >= -1 {
		return
	}
	v := &a.visits[n-1]
	if !v.hasRSSI {
		v.firstRSSI, v.hasRSSI = rssi, true
	}
	v.lastRSSI = rssi
}

const historyWindow = 10 * time.Minute

// recordJoin closes the open visit and starts a new one. It runs for every
// BSSID change, whether Jev caused it or not.
func (a *Agent) recordJoin(bssid string, at time.Time) {
	if n := len(a.visits); n > 0 && a.visits[n-1].left.IsZero() {
		a.visits[n-1].left = at
	}
	a.visits = append(a.visits, visit{bssid: bssid, joined: at})
	cut := 0
	for cut < len(a.visits)-1 && at.Sub(a.visits[cut].joined) > time.Hour {
		cut++
	}
	a.visits = a.visits[cut:]
}

// apHistory is what code counts so Jev doesn't have to: Jev 1.13 is
// documented as unreliable at counting list items.
type apHistory struct {
	joins    int           // arrivals in the last 10 min (not counting the first association)
	lastLeft time.Time     // zero if never left
	avgStay  time.Duration // over completed visits in the window
	stays    int
	last     *visit // most recent completed visit
}

func (a *Agent) historyOf(bssid string, now time.Time) apHistory {
	var h apHistory
	var total time.Duration
	for i, v := range a.visits {
		if v.bssid != bssid {
			continue
		}
		if i > 0 && now.Sub(v.joined) <= historyWindow {
			h.joins++
		}
		if !v.left.IsZero() {
			h.lastLeft = v.left
			h.last = &a.visits[i]
			if now.Sub(v.left) <= historyWindow {
				total += v.left.Sub(v.joined)
				h.stays++
			}
		}
	}
	if h.stays > 0 {
		h.avgStay = total / time.Duration(h.stays)
	}
	return h
}

func (h apHistory) words(now time.Time, current bool) string {
	var parts []string
	switch {
	case h.joins == 1:
		parts = append(parts, "roamed here once in the last 10 min")
	case h.joins > 1:
		parts = append(parts, fmt.Sprintf(
			"roamed here %d times in the last 10 min", h.joins))
	}
	if !current && !h.lastLeft.IsZero() {
		parts = append(parts, "client left this AP "+ago(now.Sub(h.lastLeft)))
	}
	if h.stays > 0 {
		parts = append(parts, fmt.Sprintf("average stay %s over %d visit(s)",
			roundDur(h.avgStay), h.stays))
	}
	if !current && h.last != nil && h.last.hasRSSI {
		parts = append(parts, fmt.Sprintf(
			"during the last stay its signal went from %d to %d dBm",
			h.last.firstRSSI, h.last.lastRSSI))
	}
	return strings.Join(parts, "; ")
}

func roundDur(d time.Duration) string {
	if d < 90*time.Second {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%.1f min", d.Minutes())
}

// roamsWithin counts BSSID changes in the window.
func (a *Agent) roamsWithin(now time.Time, d time.Duration) int {
	n := 0
	for i, v := range a.visits {
		if i > 0 && now.Sub(v.joined) <= d {
			n++
		}
	}
	return n
}

// pingPong describes a back-and-forth pattern between two APs, if the most
// recent changes alternate between the same pair. It states a fact; it does
// not block anything.
func (a *Agent) pingPong(now time.Time) string {
	var recent []visit
	for _, v := range a.visits {
		if now.Sub(v.joined) <= 5*time.Minute || v.left.IsZero() ||
			now.Sub(v.left) <= 5*time.Minute {
			recent = append(recent, v)
		}
	}
	if len(recent) < 3 {
		return ""
	}
	// Walk back from the newest visit while it alternates between two APs.
	x, y := recent[len(recent)-1].bssid, recent[len(recent)-2].bssid
	if x == y {
		return ""
	}
	n := 2
	for i := len(recent) - 3; i >= 0; i-- {
		want := x
		if (len(recent)-1-i)%2 == 1 {
			want = y
		}
		if recent[i].bssid != want {
			break
		}
		n++
	}
	switches := n - 1
	if switches < 2 {
		return ""
	}
	span := now.Sub(recent[len(recent)-n].joined)
	var stays time.Duration
	var k int
	for _, v := range recent[len(recent)-n:] {
		if !v.left.IsZero() {
			stays += v.left.Sub(v.joined)
			k++
		}
	}
	s := fmt.Sprintf("the client has switched back and forth between %s "+
		"and %s %d times in the last %s", apID(x), apID(y), switches,
		roundDur(span))
	if k > 0 {
		s += fmt.Sprintf("; average stay %s", roundDur(stays/time.Duration(k)))
	}
	// Say whether the APs being left actually faded: walking between two
	// APs looks like back-and-forth too, but there each AP fades first.
	minF, maxF, m := 0, 0, 0
	for _, v := range recent[len(recent)-n:] {
		if v.left.IsZero() || !v.hasRSSI {
			continue
		}
		f := v.fall()
		if m == 0 || f < minF {
			minF = f
		}
		if m == 0 || f > maxF {
			maxF = f
		}
		m++
	}
	switch {
	case m == 0:
	case maxF < PingPongFallDB && minF > -PingPongFallDB:
		s += fmt.Sprintf("; the signal on each AP changed by %d dB or less "+
			"before the client left it (no sign the client is moving)",
			max(maxF, -minF))
	case minF >= PingPongFallDB:
		s += fmt.Sprintf("; each time, the AP being left had fallen %d-%d dB "+
			"first (consistent with the client moving between them)",
			minF, maxF)
	default:
		s += fmt.Sprintf("; the AP being left had fallen between %d and %d "+
			"dB before each switch", minF, maxF)
	}
	return s
}
