package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jwil007/roamjev/internal/netlink"
	"github.com/jwil007/roamjev/internal/wpac"
)

// WPAS drives a real interface through wpa_supplicant's control socket,
// reusing roamctl's wpac and netlink packages.
type WPAS struct {
	C *wpac.Client
	// AllowBTM leaves 802.11v BSS Transition Management enabled, which lets
	// the AP steer the client outside the agent's control.
	AllowBTM bool
}

func (w *WPAS) Link(ctx context.Context) (Link, error) {
	st, err := w.C.GetStatus()
	if err != nil {
		return Link{}, fmt.Errorf("wpa_supplicant STATUS: %w", err)
	}
	type res struct {
		info netlink.STAInfo
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		i, err := netlink.GetStationInfo(w.C.Iface)
		ch <- res{i, err}
	}()
	var info netlink.STAInfo
	select {
	case r := <-ch:
		if r.err != nil {
			slog.Debug("GetStationInfo failed", "err", r.err)
		}
		info = r.info
	case <-time.After(3 * time.Second):
		slog.Warn("GetStationInfo timed out")
	case <-ctx.Done():
		return Link{}, ctx.Err()
	}
	rssi := info.AvgRSSIBeacon
	if rssi >= -1 {
		rssi = info.AvgRSSI
	}
	if rssi >= -1 {
		rssi = info.RSSI
	}
	return Link{
		SSID:      st.SSID,
		WPAState:  st.WPAState,
		BSSID:     info.BSSID,
		Freq:      info.Freq,
		RSSI:      rssi,
		TxMCS:     info.TxMCS,
		RxMCS:     info.RxMCS,
		TxBitrate: info.TxBitrate,
		RxBitrate: info.RxBitrate,
		TxPHY:     info.TxPHY,
		RxPHY:     info.RxPHY,
		Width:     info.ChannelWidth,
		Connected: info.ConnDuration,
	}, nil
}

func (w *WPAS) Scan(ctx context.Context, freqs []int) error {
	timeout := 4 * time.Second
	if freqs == nil {
		timeout = 15 * time.Second
	}
	return w.C.Scan(ctx, wpac.ScanParams{
		Freqs: freqs, Timeout: timeout, RetryCount: 3})
}

func (w *WPAS) ScanResults(_ context.Context, ssid string) ([]BSS, error) {
	rich, err := w.C.ScanResults(ssid)
	if err != nil {
		return nil, err
	}
	out := make([]BSS, 0, len(rich))
	for _, r := range rich {
		util, sta := -1, -1
		if r.QBSSUtil != 0 || r.QBSSStaCt != 0 {
			util = int(r.QBSSUtil) * 100 / 255
			sta = int(r.QBSSStaCt)
		}
		out = append(out, BSS{
			BSSID:             r.BSSID,
			SSID:              r.SSID,
			Freq:              r.Freq,
			Channel:           r.ChannelNum,
			Band:              r.Band.String(),
			RSSI:              r.RSSI,
			SNR:               r.SNR,
			Width:             r.ChannelWidth.String(),
			PHY:               r.PHYType.String(),
			UtilPct:           util,
			Stations:          sta,
			EstThroughputKbps: r.EstThruput,
			Age:               r.Age,
		})
	}
	return out, nil
}

func (w *WPAS) Roam(ctx context.Context, bssid string) (RoamResult, error) {
	r, err := w.C.Roam(ctx, bssid)
	if err != nil {
		if strings.Contains(err.Error(), "timed out waiting for event") {
			return RoamResult{Message: "roam timed out"}, nil
		}
		return RoamResult{}, err
	}
	return RoamResult{
		Success:  r.Success,
		Final:    r.FinalBSSID,
		Duration: r.Duration,
		Message:  r.Message,
	}, nil
}

// Prepare clears bgscan and (unless AllowBTM) disables BTM, the same way
// roamctl does, so wpa_supplicant never roams on its own.
func (w *WPAS) Prepare() (func(), error) {
	stored, err := w.C.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("wpa_supplicant GetConfig: %w", err)
	}
	btm := "1"
	if w.AllowBTM {
		btm = "0"
	}
	want := wpac.WPAConfig{SSID: stored.SSID, NetworkID: stored.NetworkID,
		BGScan: "", DisableBTM: btm}
	if stored.BGScan != want.BGScan || stored.DisableBTM != want.DisableBTM {
		if err := w.C.SetConfig(want); err != nil {
			return nil, fmt.Errorf("wpa_supplicant SetConfig: %w", err)
		}
	}
	slog.Info("wpa_supplicant autonomous roaming disabled",
		"ssid", stored.SSID, "orig_bgscan", stored.BGScan,
		"btm_disabled", btm == "1")
	return func() {
		if err := w.C.SetConfig(stored); err != nil {
			slog.Error("restoring wpa_supplicant config", "err", err)
		} else {
			slog.Info("wpa_supplicant config restored")
		}
	}, nil
}
