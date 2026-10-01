// Package linkq measures link quality to the default gateway: loss, latency,
// and jitter from ARP probes, folded into a MOS estimate, plus host-wide TCP
// retransmissions as a proxy for application-level retries.
package linkq

import "math"

// MOS estimates a voice Mean Opinion Score (1.0-4.5) from round-trip
// latency, jitter and loss using the simplified ITU-T G.107 E-model that
// most VoIP monitoring tools use:
//
//	effective = latency + 2*jitter + 10
//	R = 93.2 - effective/40          (effective < 160 ms)
//	R = 93.2 - (effective-120)/10    (otherwise)
//	R -= 2.5 * loss%
//	MOS = 1 + 0.035R + 7e-6 * R(R-60)(100-R)
//
// On a single Wi-Fi hop, latency is small, so loss and jitter dominate.
func MOS(latencyMs, jitterMs, lossPct float64) (mos, r float64) {
	eff := latencyMs + 2*jitterMs + 10
	if eff < 160 {
		r = 93.2 - eff/40
	} else {
		r = 93.2 - (eff-120)/10
	}
	r -= 2.5 * lossPct
	r = math.Max(0, math.Min(100, r))
	mos = 1 + 0.035*r + 0.000007*r*(r-60)*(100-r)
	return math.Max(1, math.Min(4.5, mos)), r
}
