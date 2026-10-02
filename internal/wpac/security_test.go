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
