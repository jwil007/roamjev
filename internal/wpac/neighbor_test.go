package wpac

import (
	"context"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeWPAS speaks enough of wpa_supplicant's control protocol for the
// neighbor report flow: replies to commands on the sender's socket and
// pushes unsolicited events to every ATTACHed socket, as the real daemon
// does.
func fakeWPAS(t *testing.T, path string, reply string, events []string) {
	t.Helper()
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	var mu sync.Mutex
	var attached []*net.UnixAddr
	go func() {
		buf := make([]byte, 4096)
		for {
			n, from, err := conn.ReadFromUnix(buf)
			if err != nil {
				return
			}
			cmd := strings.TrimSpace(string(buf[:n]))
			switch cmd {
			case "ATTACH":
				mu.Lock()
				attached = append(attached, from)
				mu.Unlock()
				_, _ = conn.WriteToUnix([]byte("OK\n"), from)
			case "NEIGHBOR_REP_REQUEST":
				_, _ = conn.WriteToUnix([]byte(reply+"\n"), from)
				if reply != "OK" {
					continue
				}
				go func() {
					time.Sleep(80 * time.Millisecond) // the AP takes a moment
					mu.Lock()
					defer mu.Unlock()
					for _, ev := range events {
						for _, a := range attached {
							_, _ = conn.WriteToUnix([]byte(ev), a)
						}
					}
				}()
			default:
				_, _ = conn.WriteToUnix([]byte("UNKNOWN COMMAND\n"), from)
			}
		}
	}()
}

func TestNeighborReportProtocol(t *testing.T) {
	dir := t.TempDir()
	fakeWPAS(t, dir+"/wlan0", "OK", []string{
		"<3>RRM-NEIGHBOR-REP-RECEIVED bssid=aa:bb:cc:dd:ee:01 info=0x8f op_class=115 chan=36 phy_type=9",
		"<3>RRM-NEIGHBOR-REP-RECEIVED bssid=aa:bb:cc:dd:ee:02 info=0x8f op_class=125 chan=149 phy_type=9",
		"<3>RRM-NEIGHBOR-REP-RECEIVED bssid=aa:bb:cc:dd:ee:03 info=0x8f op_class=81 chan=6 phy_type=7",
		"<3>RRM-NEIGHBOR-REP-RECEIVED bssid=aa:bb:cc:dd:ee:04 info=0x8f op_class=131 chan=37 phy_type=14",
		"<3>CTRL-EVENT-SCAN-STARTED ", // unrelated events are ignored
	})
	c, err := ConnectAt(dir, dir, "wlan0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	start := time.Now()
	freqs, err := c.NeighborReport(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(freqs)
	want := []int{2437, 5180, 5745, 6135}
	if !slices.Equal(freqs, want) {
		t.Fatalf("freqs = %v, want %v", freqs, want)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("took %v; should return soon after the burst ends", d)
	}
}

func TestNeighborReportUnsupported(t *testing.T) {
	dir := t.TempDir()
	fakeWPAS(t, dir+"/wlan0", "FAIL", nil)
	c, err := ConnectAt(dir, dir, "wlan0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.NeighborReport(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "802.11k") {
		t.Fatalf("want an 802.11k-unsupported error, got %v", err)
	}
}

// The AP accepts the request but never answers: wpa_supplicant times out
// and emits RRM-NEIGHBOR-REP-REQUEST-FAILED, which must end the wait.
func TestNeighborReportTimeout(t *testing.T) {
	dir := t.TempDir()
	fakeWPAS(t, dir+"/wlan0", "OK", []string{"<3>RRM-NEIGHBOR-REP-REQUEST-FAILED "})
	c, err := ConnectAt(dir, dir, "wlan0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	start := time.Now()
	_, raw, err := c.NeighborReportRaw(context.Background(), 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("want a did-not-answer error, got %v", err)
	}
	if len(raw) != 1 || time.Since(start) > time.Second {
		t.Fatalf("raw=%v after %v", raw, time.Since(start))
	}
}
