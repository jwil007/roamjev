package linkq

import (
	"math"
	"sync"
	"time"
)

// Sample is one probe: either a reply with its round-trip time, or a loss.
type Sample struct {
	At   time.Time
	RTT  time.Duration
	Lost bool
}

// TCPSample is a reading of the kernel's cumulative TCP counters.
type TCPSample struct {
	At          time.Time
	OutSegs     uint64
	RetransSegs uint64
}

// Window summarizes probes over a time range.
type Window struct {
	From, To   time.Time
	Sent       int     `json:"sent"`
	Lost       int     `json:"lost"`
	LossPct    float64 `json:"loss_pct"`
	LatencyMs  float64 `json:"latency_ms"`
	JitterMs   float64 `json:"jitter_ms"`
	MOS        float64 `json:"mos"`
	RFactor    float64 `json:"r_factor"`
	TCPOutSegs uint64  `json:"tcp_out_segs"`
	TCPRetrans uint64  `json:"tcp_retrans_segs"`
	// TCPRetransPct is retransmitted / sent segments, host-wide. Meaningless
	// with little traffic, so check TCPOutSegs before trusting it.
	TCPRetransPct float64 `json:"tcp_retrans_pct"`
	Valid         bool    `json:"valid"`
}

// Ring holds recent samples. It is safe for concurrent use.
type Ring struct {
	mu   sync.Mutex
	keep time.Duration
	s    []Sample
	tcp  []TCPSample
}

func NewRing(keep time.Duration) *Ring { return &Ring{keep: keep} }

func (r *Ring) Add(s Sample) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.s = append(r.s, s)
	cut := 0
	for cut < len(r.s) && s.At.Sub(r.s[cut].At) > r.keep {
		cut++
	}
	r.s = r.s[cut:]
}

func (r *Ring) AddTCP(t TCPSample) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tcp = append(r.tcp, t)
	cut := 0
	for cut < len(r.tcp) && t.At.Sub(r.tcp[cut].At) > r.keep {
		cut++
	}
	r.tcp = r.tcp[cut:]
}

// Last returns the window ending now.
func (r *Ring) Last(d time.Duration) Window {
	now := time.Now()
	return r.Between(now.Add(-d), now)
}

// Between summarizes samples with from <= At < to.
func (r *Ring) Between(from, to time.Time) Window {
	r.mu.Lock()
	defer r.mu.Unlock()
	w := Window{From: from, To: to}
	var sum float64
	var jsum float64
	var jn int
	prev := -1.0
	for _, s := range r.s {
		if s.At.Before(from) || !s.At.Before(to) {
			continue
		}
		w.Sent++
		if s.Lost {
			w.Lost++
			continue
		}
		ms := float64(s.RTT.Microseconds()) / 1000
		sum += ms
		if prev >= 0 {
			jsum += math.Abs(ms - prev)
			jn++
		}
		prev = ms
	}
	if w.Sent == 0 {
		return w
	}
	w.Valid = true
	w.LossPct = 100 * float64(w.Lost) / float64(w.Sent)
	if got := w.Sent - w.Lost; got > 0 {
		w.LatencyMs = sum / float64(got)
	}
	if jn > 0 {
		w.JitterMs = jsum / float64(jn)
	}
	w.MOS, w.RFactor = MOS(w.LatencyMs, w.JitterMs, w.LossPct)

	var first, last *TCPSample
	for i := range r.tcp {
		t := &r.tcp[i]
		if t.At.Before(from) || t.At.After(to) {
			continue
		}
		if first == nil {
			first = t
		}
		last = t
	}
	if first != nil && last != first && last.OutSegs >= first.OutSegs &&
		last.RetransSegs >= first.RetransSegs {
		w.TCPOutSegs = last.OutSegs - first.OutSegs
		w.TCPRetrans = last.RetransSegs - first.RetransSegs
		if w.TCPOutSegs > 0 {
			w.TCPRetransPct = 100 * float64(w.TCPRetrans) /
				float64(w.TCPOutSegs)
		}
	}
	return w
}

// Samples returns a copy of samples since t, for the UI.
func (r *Ring) Samples(since time.Time) []Sample {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Sample
	for _, s := range r.s {
		if !s.At.Before(since) {
			out = append(out, s)
		}
	}
	return out
}
