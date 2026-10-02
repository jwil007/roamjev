package agent

import (
	"context"
	"time"

	"github.com/jwil007/roamjev/internal/jev"
)

// Policy is the decider. The agent owns everything else (measuring,
// describing, executing, rails, grading, journaling), so swapping the policy
// swaps only who decides.
type Policy interface {
	Name() string
	Decide(ctx context.Context, in PolicyInput) (PolicyOutput, error)
	// Notify reports what happened after decisions, for policies that keep
	// their own state (the classic algorithm's timers and penalties).
	Notify(ev PolicyEvent)
}

type PolicyInput struct {
	Now        time.Time
	Link       Link
	Cands      []Candidate // current AP plus the strongest others
	Scan       []BSS       // full latest scan list
	ScanAt     time.Time
	State      map[string]any // the described state (what Jev reads)
	LastRoam   time.Time
	ConnChange time.Time
}

type PolicyOutput struct {
	Chosen     string // stay | scan_targeted | scan_full | roam
	Target     string // BSSID, when roaming
	Confidence float64
	// Reason is a plain-language explanation (classic policy only; Jev
	// gives probabilities instead of reasons).
	Reason string

	// Jev call details, when the policy called Jev.
	UsedJev   bool
	Questions map[string]jev.Question
	Answers   map[string]jev.Answer
	TargetIDs map[string]string // option key -> BSSID
	Model     string
	Latency   time.Duration
	Tokens    int
	CostUSD   float64
}

type PolicyEvent struct {
	Kind    string // "roam", "scan", "conn_change"
	At      time.Time
	Target  string
	Success bool
	Message string
}

// JevPolicy asks Jev.
type JevPolicy struct {
	Client  *jev.Client
	Timeout time.Duration
	// VerifyRoam tells Jev that roams are checked automatically first.
	VerifyRoam bool
}

func (p *JevPolicy) Name() string { return "jev" }

func (p *JevPolicy) Notify(PolicyEvent) {}

func (p *JevPolicy) Decide(ctx context.Context, in PolicyInput) (PolicyOutput, error) {
	qs := questions(in.Cands, p.VerifyRoam)
	out := PolicyOutput{UsedJev: true, Questions: qs,
		TargetIDs: candidateIDs(in.Cands)}
	cctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	res, err := p.Client.Evaluate(cctx, in.State, qs)
	if err != nil {
		return out, err
	}
	out.Answers = res.Answers
	out.Model = res.Model
	out.Latency = res.Latency
	out.Tokens = res.Usage.InputTokens
	out.CostUSD = res.CostUSD
	act := res.Answers["action"]
	out.Chosen, out.Confidence = act.Choice, act.Confidence
	if b, ok := out.TargetIDs[res.Answers["target"].Choice]; ok {
		out.Target = b
	}
	return out, nil
}
