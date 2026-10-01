package agent

import (
	"context"
	"time"
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
}
