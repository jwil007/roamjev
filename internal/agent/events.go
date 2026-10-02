package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/jwil007/roamjev/internal/jev"
	"github.com/jwil007/roamjev/internal/linkq"
)

// Tick is a once-per-second snapshot of the link, for the timeline.
type Tick struct {
	T             time.Time `json:"t"`
	BSSID         string    `json:"bssid"`
	Freq          int       `json:"freq"`
	RSSI          int       `json:"rssi"`
	TxMCS         int       `json:"tx_mcs"`
	RxMCS         int       `json:"rx_mcs"`
	TxMbps        float64   `json:"tx_mbps"`
	RxMbps        float64   `json:"rx_mbps"`
	MOS           float64   `json:"mos"`
	LossPct       float64   `json:"loss_pct"`
	LatencyMs     float64   `json:"latency_ms"`
	JitterMs      float64   `json:"jitter_ms"`
	TCPRetransPct float64   `json:"tcp_retrans_pct"`
	TCPOutSegs    uint64    `json:"tcp_out_segs"`
	Connected     bool      `json:"connected"`
}

// Decision is one Jev call: exactly what it saw, what it answered, and what
// the agent did with the answer.
type Decision struct {
	ID        int                     `json:"id"`
	T         time.Time               `json:"t"`
	Trigger   string                  `json:"trigger"`
	State     json.RawMessage         `json:"state"`
	Questions map[string]jev.Question `json:"questions"`
	Answers   map[string]jev.Answer   `json:"answers,omitempty"`
	Model     string                  `json:"model,omitempty"`
	LatencyMs float64                 `json:"latency_ms"`
	Tokens    int                     `json:"tokens"`
	CostUSD   float64                 `json:"cost_usd"`
	Err       string                  `json:"err,omitempty"`
	// Chosen is Jev's top action; Target the BSSID it picked (if any).
	Chosen     string  `json:"chosen"`
	Confidence float64 `json:"confidence"`
	Target     string  `json:"target,omitempty"`
	// Executed is what actually happened; Blocked explains any difference.
	Executed string `json:"executed"`
	Blocked  string `json:"blocked,omitempty"`
}

// Action is a scan or roam the agent carried out.
type Action struct {
	T          time.Time `json:"t"`
	DecisionID int       `json:"decision_id"`
	Kind       string    `json:"kind"`
	From       string    `json:"from,omitempty"`
	Target     string    `json:"target,omitempty"`
	Freqs      []int     `json:"freqs,omitempty"`
	DurationMs float64   `json:"duration_ms"`
	Success    bool      `json:"success"`
	Message    string    `json:"message,omitempty"`
	Found      int       `json:"found,omitempty"`
}

// Outcome grades a roam.
//
// Pre/Post compare 15 s before with 3-18 s after. That is all a real radio
// can measure, but it is optimistic: roams usually follow a dip, and MOS
// tends to recover after a dip anyway. Baseline (15-75 s before) shows
// whether the roam beat normal conditions or merely ended a dip.
//
// In the simulator, Stay is the link quality the client would have had over
// the same window (roam start to 18 s after) had it not roamed, and the
// verdict compares Actual against Stay instead.
type Outcome struct {
	T          time.Time     `json:"t"`
	DecisionID int           `json:"decision_id"`
	From       string        `json:"from"`
	To         string        `json:"to"`
	Pre        linkq.Window  `json:"pre"`
	Post       linkq.Window  `json:"post"`
	Baseline   linkq.Window  `json:"baseline"`
	Disruption linkq.Window  `json:"disruption"`
	Actual     linkq.Window  `json:"actual"`
	Stay       *linkq.Window `json:"stay,omitempty"`
	RSSIBefore int           `json:"rssi_before"`
	RSSIAfter  int           `json:"rssi_after"`
	// MOSDelta is Post - Pre; GainVsStay is Actual - Stay (sim only).
	MOSDelta   float64 `json:"mos_delta"`
	GainVsStay float64 `json:"gain_vs_stay"`
	// Verdict uses the best grade available ("vs stay" in the simulator).
	// VerdictBA is always the before/after grade; it is what Jev is shown,
	// because a real radio couldn't know the counterfactual.
	Verdict   string `json:"verdict"`
	VerdictBA string `json:"verdict_ba"`
	Basis     string `json:"basis"`
}

// Note is a free-form event (external roam, disconnect, budget hit...).
type Note struct {
	T    time.Time `json:"t"`
	Kind string    `json:"kind"`
	Text string    `json:"text"`
}

// Info is static run metadata.
type Info struct {
	Iface     string    `json:"iface"`
	Mode      string    `json:"mode"`
	Model     string    `json:"model"`
	Started   time.Time `json:"started"`
	Interval  string    `json:"interval"`
	BudgetUSD float64   `json:"budget_usd"`
	Journal   string    `json:"journal"`
	Version   string    `json:"version"`
	Replay    bool      `json:"replay,omitempty"`
}

// Status is the live summary shown in the UI header.
type Status struct {
	SSID       string  `json:"ssid"`
	BSSID      string  `json:"bssid"`
	Freq       int     `json:"freq"`
	Band       string  `json:"band"`
	Channel    int     `json:"channel"`
	WPAState   string  `json:"wpa_state"`
	Busy       string  `json:"busy"`
	Calls      int     `json:"calls"`
	Errors     int     `json:"errors"`
	SpentUSD   float64 `json:"spent_usd"`
	AvgLatency float64 `json:"avg_latency_ms"`
	Paused     bool    `json:"paused"`
	Gateway    string  `json:"gateway"`
}

// Event is what the journal and the UI stream carry.
type Event struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

// Store keeps recent events in memory, appends them to a JSONL journal, and
// fans them out to UI subscribers.
type Store struct {
	mu        sync.Mutex
	info      Info
	status    Status
	ticks     []Tick
	decisions []Decision
	actions   []Action
	outcomes  []Outcome
	notes     []Note
	cands     []Candidate
	subs      map[chan Event]struct{}
	w         *bufio.Writer
	f         *os.File
}

func NewStore(journal string) (*Store, error) {
	s := &Store{subs: map[chan Event]struct{}{}}
	if journal != "" {
		f, err := os.OpenFile(journal,
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return nil, fmt.Errorf("open journal: %w", err)
		}
		s.f = f
		s.w = bufio.NewWriter(f)
	}
	return s, nil
}

func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.w != nil {
		_ = s.w.Flush()
		_ = s.f.Close()
		s.w = nil
	}
}

func trim[T any](xs []T, n int) []T {
	if len(xs) > n {
		return xs[len(xs)-n:]
	}
	return xs
}

func (s *Store) emit(e Event, journal bool) {
	if journal && s.w != nil {
		b, err := json.Marshal(e)
		if err == nil {
			_, _ = s.w.Write(append(b, '\n'))
			_ = s.w.Flush()
		}
	}
	for ch := range s.subs {
		select {
		case ch <- e:
		default: // slow client; it will resync from the snapshot
		}
	}
}

func (s *Store) SetInfo(i Info) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.info = i
	s.emit(Event{"info", i}, true)
}

func (s *Store) SetStatus(st Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = st
	s.emit(Event{"status", st}, false)
}

func (s *Store) AddTick(t Tick) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ticks = trim(append(s.ticks, t), 7200)
	s.emit(Event{"tick", t}, true)
}

func (s *Store) AddDecision(d Decision) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.decisions = trim(append(s.decisions, d), 3000)
	s.emit(Event{"decision", d}, true)
}

func (s *Store) AddAction(a Action) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.actions = trim(append(s.actions, a), 2000)
	s.emit(Event{"action", a}, true)
}

func (s *Store) AddOutcome(o Outcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outcomes = trim(append(s.outcomes, o), 1000)
	s.emit(Event{"outcome", o}, true)
}

func (s *Store) AddNote(kind, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := Note{T: time.Now(), Kind: kind, Text: text}
	s.notes = trim(append(s.notes, n), 1000)
	s.emit(Event{"note", n}, true)
}

func (s *Store) SetCandidates(c []Candidate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cands = c
	s.emit(Event{"candidates", c}, true)
}

// Snapshot is everything the UI needs on first load.
type Snapshot struct {
	Info       Info        `json:"info"`
	Status     Status      `json:"status"`
	Ticks      []Tick      `json:"ticks"`
	Decisions  []Decision  `json:"decisions"`
	Actions    []Action    `json:"actions"`
	Outcomes   []Outcome   `json:"outcomes"`
	Notes      []Note      `json:"notes"`
	Candidates []Candidate `json:"candidates"`
}

func (s *Store) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Snapshot{
		Info:       s.info,
		Status:     s.status,
		Ticks:      append([]Tick(nil), s.ticks...),
		Decisions:  append([]Decision(nil), s.decisions...),
		Actions:    append([]Action(nil), s.actions...),
		Outcomes:   append([]Outcome(nil), s.outcomes...),
		Notes:      append([]Note(nil), s.notes...),
		Candidates: append([]Candidate(nil), s.cands...),
	}
}

func (s *Store) Subscribe() (chan Event, func()) {
	ch := make(chan Event, 256)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}
}

// LoadJournal replays a JSONL journal into a fresh store (no journal output),
// for reviewing a past run in the UI.
func LoadJournal(path string) (*Store, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	s, _ := NewStore("")
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var raw struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal(sc.Bytes(), &raw) != nil {
			continue
		}
		switch raw.Type {
		case "info":
			var v Info
			if json.Unmarshal(raw.Data, &v) == nil {
				v.Replay = true
				s.info = v
			}
		case "tick":
			var v Tick
			if json.Unmarshal(raw.Data, &v) == nil {
				s.ticks = append(s.ticks, v)
			}
		case "decision":
			var v Decision
			if json.Unmarshal(raw.Data, &v) == nil {
				s.decisions = append(s.decisions, v)
				s.status.Calls++
				s.status.SpentUSD += v.CostUSD
			}
		case "action":
			var v Action
			if json.Unmarshal(raw.Data, &v) == nil {
				s.actions = append(s.actions, v)
			}
		case "outcome":
			var v Outcome
			if json.Unmarshal(raw.Data, &v) == nil {
				s.outcomes = append(s.outcomes, v)
			}
		case "note":
			var v Note
			if json.Unmarshal(raw.Data, &v) == nil {
				s.notes = append(s.notes, v)
			}
		case "candidates":
			var v []Candidate
			if json.Unmarshal(raw.Data, &v) == nil {
				s.cands = v
			}
		}
	}
	// Rebuild the header status from the recording.
	var lat float64
	var n int
	for _, d := range s.decisions {
		if d.Err != "" {
			s.status.Errors++
		} else if d.LatencyMs > 0 {
			lat += d.LatencyMs
			n++
		}
	}
	if n > 0 {
		s.status.AvgLatency = lat / float64(n)
	}
	if len(s.ticks) > 0 {
		t := s.ticks[len(s.ticks)-1]
		s.status.BSSID, s.status.Freq = t.BSSID, t.Freq
		s.status.Band, s.status.Channel = bandChannel(t.Freq)
	}
	s.status.Gateway = "replay"
	return s, sc.Err()
}
