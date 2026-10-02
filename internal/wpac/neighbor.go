package wpac

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// NeighborReport asks the current AP for its 802.11k neighbor report and
// returns the channel frequencies it lists. wpa_supplicant answers the
// request with OK, then emits one RRM-NEIGHBOR-REP-RECEIVED event per
// neighbor (or RRM-NEIGHBOR-REP-FAILED). APs without 802.11k make the
// request fail.
func (c *Client) NeighborReport(ctx context.Context) ([]int, error) {
	freqs, _, err := c.NeighborReportRaw(ctx, 2*time.Second)
	return freqs, err
}

// NeighborReportRaw is NeighborReport that also returns every event seen
// during the wait, for diagnosing what a real AP and wpa_supplicant send.
func (c *Client) NeighborReportRaw(ctx context.Context, wait time.Duration) ([]int, []string, error) {
	var raw []string
	lctx, cancel := context.WithCancel(ctx)
	defer cancel()
	events, errc := c.listenEvents(lctx)
	out, err := c.cmd("NEIGHBOR_REP_REQUEST")
	if err != nil {
		return nil, nil, fmt.Errorf("NEIGHBOR_REP_REQUEST: %w", err)
	}
	if strings.TrimSpace(string(out)) != "OK" {
		return nil, nil, fmt.Errorf("NEIGHBOR_REP_REQUEST: %s (AP may not support 802.11k)",
			strings.TrimSpace(string(out)))
	}
	var freqs []int
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	quiet := time.NewTimer(time.Hour)
	defer quiet.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, raw, ctx.Err()
		case err := <-errc:
			return nil, raw, err
		case <-deadline.C:
			return freqs, raw, nil
		case <-quiet.C:
			return freqs, raw, nil // the burst of reports is over
		case ev := <-events:
			raw = append(raw, strings.TrimSpace(ev))
			// wpa_supplicant's name is RRM-NEIGHBOR-REP-REQUEST-FAILED; accept
			// the shorter form too.
			if strings.Contains(ev, "RRM-NEIGHBOR-REP-REQUEST-FAILED") ||
				strings.Contains(ev, "RRM-NEIGHBOR-REP-FAILED") {
				return nil, raw, fmt.Errorf("neighbor report failed (AP did not answer): %s",
					strings.TrimSpace(ev))
			}
			if !strings.Contains(ev, "RRM-NEIGHBOR-REP-RECEIVED") {
				continue
			}
			if f, ok := parseNeighbor(ev); ok && !slices.Contains(freqs, f) {
				freqs = append(freqs, f)
			}
			quiet.Reset(300 * time.Millisecond)
		}
	}
}

// parseNeighbor extracts the channel from an event like
// "<3>RRM-NEIGHBOR-REP-RECEIVED bssid=aa:bb:cc:dd:ee:ff info=0x8f
// op_class=128 chan=36 phy_type=9".
func parseNeighbor(ev string) (int, bool) {
	var op, ch int
	for _, f := range strings.Fields(ev) {
		if v, ok := strings.CutPrefix(f, "op_class="); ok {
			op, _ = strconv.Atoi(v)
		}
		if v, ok := strings.CutPrefix(f, "chan="); ok {
			ch, _ = strconv.Atoi(v)
		}
	}
	if ch == 0 {
		return 0, false
	}
	switch {
	case op >= 81 && op <= 84: // 2.4 GHz
		if ch == 14 {
			return 2484, true
		}
		return 2407 + 5*ch, true
	case op >= 131 && op <= 137: // 6 GHz
		return 5950 + 5*ch, true
	case op >= 115 && op <= 130, op == 0: // 5 GHz (op_class 0: guess by channel)
		if op == 0 && ch <= 14 {
			return 2407 + 5*ch, true
		}
		return 5000 + 5*ch, true
	}
	return 0, false
}
