package sim

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"time"
)

// A scenario is a whole environment: AP layout, how busy each AP gets, the
// network's security (which sets roam time), where the client walks, and
// what it's doing. Everything random here draws from the world stream, so a
// seed reproduces the same environment for every decider.

type radioSpec struct {
	band    string
	channel int
	width   string
	phy     string
	txPower float64 // dBm
}

type apSpec struct {
	x, y     float64
	radios   []radioSpec
	baseUtil float64 // % airtime busy on its 5 GHz radio
}

type waypoint struct {
	x, y               float64
	pauseMin, pauseMax float64 // seconds spent here before moving on
}

type phase struct {
	kind       string  // idle | call | download
	weight     float64 // relative chance of picking this phase
	minS, maxS float64
}

type scenario struct {
	name     string
	describe string
	exponent float64 // log-distance path loss exponent
	shadow   float64 // shadowing random-walk step (dB)
	aps      []apSpec
	// hot returns extra utilization (percentage points) for AP i at time t:
	// crowds, a lecture letting out, a congested meeting room.
	hot      func(i int, t time.Duration) float64
	security string
	ft       bool
	// roamMs draws one roam's interruption.
	roamMs     func(r *rand.Rand) float64
	path       []waypoint
	speed      float64 // m/s
	pauseEvery float64 // mean seconds between unplanned stops while walking (0 = none)
	traffic    []phase
	// nrCoverage is how complete the APs' 802.11k neighbor reports are:
	// 0 = not supported, 1 = every nearby AP listed. Vendor support is
	// mixed, so most scenarios list only some neighbors.
	nrCoverage float64
}

// Roam interruption by security type. FT (802.11r) skips the handshake;
// PSK without FT does a 4-way handshake; 802.1X without FT does a full EAP
// exchange with the RADIUS server, which is slow and occasionally very slow.
func roamFT(r *rand.Rand) float64  { return 40 + r.Float64()*40 }
func roamPSK(r *rand.Rand) float64 { return 110 + r.Float64()*110 }
func roamEAP(r *rand.Rand) float64 {
	if r.Float64() < 0.07 {
		return 1500 + r.Float64()*1000
	}
	return 350 + r.Float64()*550
}

var dualBand56 = func(ch5, ch6 int) []radioSpec {
	return []radioSpec{
		{"5GHz", ch5, "80MHz", "802.11ax", 20},
		{"6GHz", ch6, "160MHz", "802.11be", 19},
	}
}

func scenarioByName(name string) (*scenario, error) {
	switch name {
	case "", "walk", "hallway":
		return hallway(), nil
	case "boundary":
		return boundary(), nil
	case "office":
		return office(), nil
	case "convention":
		return convention(), nil
	case "hospital":
		return hospital(), nil
	case "corridor":
		return corridor(), nil
	}
	return nil, fmt.Errorf("unknown sim scenario %q (%s)", name,
		strings.Join(ScenarioNames, ", "))
}

// ScenarioNames lists the available simulator environments.
var ScenarioNames = []string{"hallway", "boundary", "office", "convention", "hospital", "corridor"}

func hallway() *scenario {
	s := &scenario{
		name:       "hallway",
		nrCoverage: 0, // no 802.11k
		describe:   "80 m hallway, four dual-band APs plus a 2.4 GHz AP; one AP congests a minute out of every three; FT-PSK",
		exponent:   3.0, shadow: 0.6,
		security: "WPA2-PSK", ft: true, roamMs: roamFT,
		speed: 1.2, pauseEvery: 40,
		path: []waypoint{{-4, 0, 0, 3}, {82, 0, 0, 3}},
		traffic: []phase{{"idle", 3, 20, 60}, {"call", 4, 40, 120},
			{"download", 3, 20, 60}},
	}
	ch5 := []int{36, 100, 149, 52}
	ch6 := []int{37, 85, 133, 181}
	for i, x := range []float64{0, 26, 52, 78} {
		s.aps = append(s.aps, apSpec{x: x, y: 4, baseUtil: 20,
			radios: dualBand56(ch5[i], ch6[i])})
	}
	s.aps = append(s.aps, apSpec{x: 39, y: 4, baseUtil: 25,
		radios: []radioSpec{{"2.4GHz", 6, "20MHz", "802.11n", 17}}})
	s.hot = func(i int, t time.Duration) float64 {
		if i == 2 && int(t.Minutes())%3 == 1 {
			return 68
		}
		return 0
	}
	return s
}

func boundary() *scenario {
	return &scenario{
		name:       "boundary",
		nrCoverage: 0,
		describe:   "standing still midway between two identical 5 GHz APs at about -76 dBm each; FT-PSK",
		exponent:   3.0, shadow: 2.0,
		security: "WPA2-PSK", ft: true, roamMs: roamFT,
		path: []waypoint{{45, 0, 1e9, 1e9}},
		aps: []apSpec{
			{x: 0, y: 4, baseUtil: 20, radios: []radioSpec{{"5GHz", 36, "80MHz", "802.11ax", 20}}},
			{x: 90, y: 4, baseUtil: 20, radios: []radioSpec{{"5GHz", 149, "80MHz", "802.11ax", 20}}},
		},
		hot: func(int, time.Duration) float64 { return 0 },
		traffic: []phase{{"idle", 3, 20, 60}, {"call", 4, 40, 120},
			{"download", 3, 20, 60}},
	}
}

// office: six dual-band APs over a 54x30 m floor, lightly used, FT-PSK. The
// client mostly sits at a desk, with walks to meeting rooms and the kitchen.
func office() *scenario {
	s := &scenario{
		name:       "office",
		nrCoverage: 1, // managed office network, complete lists
		describe:   "small office, six dual-band APs, lightly loaded (10-30%), FT-PSK; mostly at a desk with occasional walks",
		exponent:   3.3, shadow: 0.8,
		security: "WPA2-PSK", ft: true, roamMs: roamFT,
		speed: 1.2,
		path: []waypoint{
			{6, 24, 90, 200},  // desk
			{30, 4, 120, 300}, // meeting room
			{6, 24, 120, 240}, // desk
			{50, 26, 40, 90},  // kitchen
			{8, 22, 150, 300}, // desk
			{44, 6, 60, 150},  // other meeting room
		},
		traffic: []phase{{"idle", 4, 30, 120}, {"call", 3, 60, 240},
			{"download", 3, 20, 90}},
	}
	ch5 := []int{36, 149, 100, 44, 157, 108}
	ch6 := []int{37, 85, 133, 181, 213, 21}
	i := 0
	for _, y := range []float64{6, 24} {
		for _, x := range []float64{8, 27, 46} {
			s.aps = append(s.aps, apSpec{x: x, y: y, baseUtil: 12 + float64(i%3)*6,
				radios: dualBand56(ch5[i], ch6[i])})
			i++
		}
	}
	s.hot = func(i int, t time.Duration) float64 {
		// The big meeting room AP (index 1) fills up for a meeting.
		if i == 1 && int(t.Minutes())%6 >= 4 {
			return 30
		}
		return 0
	}
	return s
}

// convention: 24 APs on a 6x4 grid over a 120x80 m hall, 5 GHz + 2.4 GHz,
// heavily loaded with roaming crowd hotspots, PSK without FT. The client
// walks the aisles slowly and stops at booths.
func convention() *scenario {
	s := &scenario{
		name:       "convention",
		nrCoverage: 0.5,
		describe:   "convention hall, 24 APs, busy (55-80% base load) with moving crowd hotspots near 95%, WPA2-PSK without FT; slow walk with booth stops",
		exponent:   2.7, shadow: 1.0,
		security: "WPA2-PSK", ft: false, roamMs: roamPSK,
		speed: 0.8, pauseEvery: 25,
		path: []waypoint{
			{10, 10, 20, 60}, {110, 10, 20, 60}, {110, 40, 20, 60},
			{10, 40, 20, 60}, {10, 70, 20, 60}, {110, 70, 20, 60},
		},
		traffic: []phase{{"idle", 3, 20, 60}, {"call", 5, 60, 180},
			{"download", 2, 15, 45}},
	}
	// Channel reuse plan: 5 GHz cycles through 9 channels, so co-channel
	// neighbors exist, as they do in real dense deployments.
	reuse := []int{36, 52, 100, 116, 149, 44, 60, 108, 157}
	i := 0
	for row := 0; row < 4; row++ {
		for col := 0; col < 6; col++ {
			s.aps = append(s.aps, apSpec{
				x: 10 + float64(col)*20, y: 10 + float64(row)*20,
				baseUtil: 55 + float64((i*37)%26),
				radios: []radioSpec{
					{"5GHz", reuse[i%len(reuse)], "40MHz", "802.11ax", 17},
					{"2.4GHz", []int{1, 6, 11}[i%3], "20MHz", "802.11ax", 14},
				},
			})
			i++
		}
	}
	s.hot = func(i int, t time.Duration) float64 {
		// A crowd hotspot drifts across the floor every few minutes.
		center := math.Mod(t.Seconds()/20, 24)
		d := math.Abs(float64(i) - center)
		if d < 2.5 {
			return 25 * (1 - d/2.5)
		}
		return 0
	}
	return s
}

// hospital: a 180 m corridor, 5 GHz + 2.4 GHz APs every 30 m alternating
// sides, moderately used, 802.1X without FT (slow roams). A nurse with a
// voice badge walks the corridor and stops in rooms.
func hospital() *scenario {
	s := &scenario{
		name:       "hospital",
		nrCoverage: 0.6,
		describe:   "hospital corridor, APs every 30 m, moderate load (25-45%), 802.1X without FT (roams 0.35-1 s, sometimes 2+ s); nurse on a voice badge",
		exponent:   3.4, shadow: 0.8,
		security: "802.1X/EAP", ft: false, roamMs: roamEAP,
		speed: 1.3,
		path: []waypoint{
			{0, 0, 20, 60}, {45, 3, 30, 90}, {95, 0, 20, 60},
			{150, -3, 30, 90}, {180, 0, 20, 40}, {120, 3, 30, 90},
			{60, -3, 30, 90},
		},
		traffic: []phase{{"idle", 3, 20, 60}, {"call", 7, 60, 240}},
	}
	ch5 := []int{36, 149, 52, 157, 100, 44, 116}
	for i := 0; i < 7; i++ {
		y := 4.0
		if i%2 == 1 {
			y = -4
		}
		s.aps = append(s.aps, apSpec{x: float64(i) * 30, y: y,
			baseUtil: 25 + float64((i*13)%20),
			radios: []radioSpec{
				{"5GHz", ch5[i], "40MHz", "802.11ac", 17},
				{"2.4GHz", []int{1, 6, 11}[i%3], "20MHz", "802.11n", 14},
			}})
	}
	s.hot = func(int, time.Duration) float64 { return 0 }
	return s
}

// Scan cost. Active scanning sends a probe and listens briefly; DFS
// channels (5 GHz 52-144) must be scanned passively, listening for a beacon.
// The radio is off its home channel for the whole dwell.
func dwellMs(freq int) float64 {
	const switchMs = 5
	if freq >= 5260 && freq <= 5720 { // DFS
		return 100 + switchMs
	}
	return 30 + switchMs
}

// fullScanFreqs is a typical tri-band sweep: 2.4 GHz 1-11, all 5 GHz
// 20 MHz channels, and the 15 6 GHz preferred scanning channels.
func fullScanFreqs() []int {
	var f []int
	for ch := 1; ch <= 11; ch++ {
		f = append(f, 2407+5*ch)
	}
	for _, ch := range []int{36, 40, 44, 48, 52, 56, 60, 64, 100, 104, 108, 112,
		116, 120, 124, 128, 132, 136, 140, 144, 149, 153, 157, 161, 165} {
		f = append(f, 5000+5*ch)
	}
	for ch := 5; ch <= 229; ch += 16 {
		f = append(f, 5950+5*ch)
	}
	slices.Sort(f)
	return f
}

// corridor: a 320 m corridor with 5 GHz APs every 40 m, each on its own
// channel, and no 802.11k. Walking end to end takes the client far beyond
// what its opening scan could hear; quick and known scans only revisit
// known channels, so a full scan is the only way to find the APs ahead.
func corridor() *scenario {
	s := &scenario{
		name:     "corridor",
		describe: "320 m corridor, 5 GHz APs every 40 m on distinct channels, no 802.11k, FT-PSK; walking end to end, so only full scans find the APs ahead",
		exponent: 3.2, shadow: 0.8,
		security: "WPA2-PSK", ft: true, roamMs: roamFT,
		speed: 1.4, pauseEvery: 60,
		nrCoverage: 0,
		path:       []waypoint{{0, 0, 5, 15}, {320, 0, 5, 15}},
		traffic: []phase{{"idle", 3, 20, 60}, {"call", 5, 60, 180},
			{"download", 2, 20, 60}},
	}
	ch5 := []int{36, 149, 52, 157, 100, 44, 116, 161, 60}
	for i := 0; i < 9; i++ {
		y := 3.0
		if i%2 == 1 {
			y = -3
		}
		s.aps = append(s.aps, apSpec{x: float64(i) * 40, y: y,
			baseUtil: 20 + float64((i*11)%15),
			radios:   []radioSpec{{"5GHz", ch5[i], "80MHz", "802.11ax", 20}}})
	}
	s.hot = func(int, time.Duration) float64 { return 0 }
	return s
}
