package wpac

import "encoding/binary"

// parseRSNAKMs returns the AKM suite types (for OUI 00-0F-AC) listed in an
// RSN element: version(2) group cipher(4) pairwise count(2) pairwise
// suites(4n) AKM count(2) AKM suites(4m) ...
func parseRSNAKMs(v []byte) []uint8 {
	if len(v) < 8 {
		return nil
	}
	off := 6 // skip version and group cipher
	n := int(binary.LittleEndian.Uint16(v[off:]))
	off += 2 + 4*n
	if off+2 > len(v) {
		return nil
	}
	m := int(binary.LittleEndian.Uint16(v[off:]))
	off += 2
	var akms []uint8
	for i := 0; i < m && off+4 <= len(v); i++ {
		if v[off] == 0x00 && v[off+1] == 0x0f && v[off+2] == 0xac {
			akms = append(akms, v[off+3])
		}
		off += 4
	}
	return akms
}

// Security names the strongest authentication an AP offers and whether it
// supports 802.11r fast transition, which largely sets how long a roam
// interrupts traffic: FT avoids a full handshake (or a full 802.1X/EAP
// exchange) on every roam.
func Security(akms []uint8, mobilityDomain bool) (name string, ft bool) {
	has := map[uint8]bool{}
	for _, a := range akms {
		has[a] = true
	}
	ft = mobilityDomain || has[3] || has[4] || has[9] || has[13] || has[25]
	switch {
	case has[1] || has[3] || has[5] || has[12] || has[13]:
		name = "802.1X/EAP"
	case has[8] || has[9] || has[24] || has[25]:
		name = "WPA3-SAE"
	case has[2] || has[4] || has[6]:
		name = "WPA2-PSK"
	case has[18]:
		name = "OWE"
	case len(akms) == 0:
		name = "open"
	default:
		name = "other"
	}
	return name, ft
}
