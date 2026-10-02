package wpac

import "testing"

func TestRSN(t *testing.T) {
	// RSN v1, CCMP group, 1 pairwise (CCMP), 2 AKMs: PSK (2) and FT-PSK (4).
	v := []byte{1, 0, 0x00, 0x0f, 0xac, 4, 1, 0, 0x00, 0x0f, 0xac, 4,
		2, 0, 0x00, 0x0f, 0xac, 2, 0x00, 0x0f, 0xac, 4, 0, 0}
	akms := parseRSNAKMs(v)
	if len(akms) != 2 || akms[0] != 2 || akms[1] != 4 {
		t.Fatalf("akms = %v", akms)
	}
	if name, ft := Security(akms, false); name != "WPA2-PSK" || !ft {
		t.Fatalf("Security = %s %v", name, ft)
	}
	if name, ft := Security([]uint8{1}, false); name != "802.1X/EAP" || ft {
		t.Fatalf("EAP = %s %v", name, ft)
	}
	if akms := parseRSNAKMs(v[:9]); akms != nil {
		t.Fatalf("truncated element parsed: %v", akms)
	}
}

func TestParseNeighbor(t *testing.T) {
	for _, c := range []struct {
		ev   string
		want int
	}{
		{"<3>RRM-NEIGHBOR-REP-RECEIVED bssid=aa:bb:cc:dd:ee:ff info=0x8f op_class=115 chan=36 phy_type=9", 5180},
		{"<3>RRM-NEIGHBOR-REP-RECEIVED bssid=aa:bb:cc:dd:ee:01 info=0x8f op_class=81 chan=6 phy_type=7", 2437},
		{"<3>RRM-NEIGHBOR-REP-RECEIVED bssid=aa:bb:cc:dd:ee:02 info=0x8f op_class=131 chan=37 phy_type=14", 6135},
	} {
		if got, ok := parseNeighbor(c.ev); !ok || got != c.want {
			t.Errorf("parseNeighbor(%q) = %d, %v", c.ev, got, ok)
		}
	}
}
