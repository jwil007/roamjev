package agent

import (
	"context"
	"time"

	"github.com/jwil007/roamjev/internal/linkq"
)

// Link is the current association as seen by the driver.
type Link struct {
	SSID      string
	BSSID     string
	Freq      int
	WPAState  string
	RSSI      int
	TxMCS     int
	RxMCS     int
	TxBitrate int // bits/s
	RxBitrate int // bits/s
	TxPHY     string
	RxPHY     string
	Width     string
	Connected time.Duration
	// KeyMgmt is the association's key management as wpa_supplicant
	// reports it (FT-PSK, WPA2-PSK, SAE, ...); FT-* means 802.11r roams.
	KeyMgmt string
	// UtilPct is the current AP's channel utilization when the radio knows
	// it (the simulator does); -1 or 0 means unknown, and the agent falls
	// back to the AP's QBSS Load from the scan cache.
	UtilPct int
}

// BSS is one scan result.
type BSS struct {
	BSSID    string
	SSID     string
	Freq     int
	Channel  int
	Band     string
	RSSI     int
	SNR      int
	Width    string
	PHY      string
	UtilPct  int // -1 when the AP doesn't advertise QBSS Load
	Stations int // -1 when unknown
	// EstThroughputKbps is wpa_supplicant's estimate from RSSI, width and PHY.
	EstThroughputKbps int
	Age               time.Duration
	// Security is the strongest auth the AP offers (WPA2-PSK, WPA3-SAE,
	// 802.1X/EAP, open); FT is 802.11r support. Together they largely set
	// how long a roam to this AP interrupts traffic.
	Security string
	FT       bool
}

type RoamResult struct {
	Success  bool
	Final    string
	Duration time.Duration
	Message  string
}

// Radio is everything the agent does to the Wi-Fi interface. The live
// implementation drives wpa_supplicant; the simulator fakes a walk between APs.
type Radio interface {
	Link(ctx context.Context) (Link, error)
	// Scan scans freqs, or every channel when freqs is nil.
	Scan(ctx context.Context, freqs []int) error
	ScanResults(ctx context.Context, ssid string) ([]BSS, error)
	Roam(ctx context.Context, bssid string) (RoamResult, error)
	// Prepare stops wpa_supplicant roaming on its own; the returned func
	// restores the original settings.
	Prepare() (restore func(), err error)
	// Counters returns cumulative interface bytes, for measuring what the
	// client is doing (idle, light, heavy traffic).
	Counters() (rx, tx uint64, err error)
}

// NeighborReporter returns the channels of APs the current AP lists in
// its 802.11k neighbor report. With it, a targeted scan can find APs on
// channels the client hasn't seen yet, which otherwise needs a full scan
// (5 and 6 GHz neighbors are usually on different channels).
type NeighborReporter interface {
	NeighborFreqs(ctx context.Context) ([]int, error)
}

// ActivityReporter is implemented by the simulator, which knows what the
// client is doing (idle, call, download). Used only for grading: Jev sees
// measured traffic rates, never this ground truth.
type ActivityReporter interface {
	Activity() string
}

// Counterfactual is implemented by radios (the simulator) that know what
// link quality the client would have measured had it stayed on an AP. It
// lets roams be graded against staying instead of against the pre-roam dip,
// which flatters almost every roam (regression to the mean).
type Counterfactual interface {
	StayQuality(bssid string, from, to time.Time) (linkq.Window, bool)
}
