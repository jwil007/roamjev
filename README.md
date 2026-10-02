# roamjev

An experiment: hand **every Wi-Fi roaming and scanning decision** to
[Jev](https://docs.typesafe.ai), TypeSafe AI's "System One" decision model, and
measure whether it keeps link quality high.

roamjev is a fork of [roamctl](https://github.com/jwil007/roamctl). It reuses
roamctl's backend (the wpa_supplicant control-interface client, nl80211 station
stats, and the bgscan/BTM takeover), but replaces roamctl's tiers, score
weights and deltas with a loop where Jev decides:

- **when to scan** (and whether a quick targeted scan or a full sweep),
- **whether to roam**, and
- **which AP to roam to**.

> [!WARNING]
> Lab tool. It deliberately removes the hand-tuned roaming logic, so expect
> odd choices — that's what it's for. Decisions depend on a cloud API: if the
> link is too broken to reach it, the agent simply does nothing.

## How it works

```
         every 3 s, and right after every scan
 ┌──────────────────────────────────────────────────────────────┐
 │ observe   nl80211: RSSI, MCS, bitrates   ARP probes: loss,   │
 │           latency, jitter -> MOS         /proc/net/snmp: TCP │
 │           retransmits    last scan results    roam history   │
 ├──────────────────────────────────────────────────────────────┤
 │ describe  JSON state: raw numbers + code-computed facts      │
 │           ("+9 dB vs current", "falling 7 dB over 10s",      │
 │           "MOS 3.4: poor", "last roam here: MOS 3.1 -> 4.3") │
 ├──────────────────────────────────────────────────────────────┤
 │ ask Jev   one call, questions evaluated in parallel:         │
 │           action  (choice) stay | scan_targeted | scan_full |│
 │                            roam                              │
 │           target  (choice) ap_xxxxxx ... | none              │
 │           urgency (score)  4 levels                          │
 │           link_degraded / better_ap_available /              │
 │           scan_data_stale (noul, yes-probability)            │
 ├──────────────────────────────────────────────────────────────┤
 │ act       do what Jev chose, unless a rail blocks it         │
 │           (every block is logged with its reason)            │
 └──────────────────────────────────────────────────────────────┘
```

### Ground rules: keeping it Jev

This experiment tests a different paradigm, not a better hand-tuned
algorithm (roamctl already is one). So:

1. **Jev makes every decision.** Code never applies a threshold that
   triggers or blocks a scan or roam, beyond minimal visible rails (roam
   and scan gaps, a valid target, an optional pre-roam staleness check
   that can only re-ask Jev, never decide to stay).
2. **Improvements go through Jev's channels:** facts and domain knowledge
   in the briefing, clearer presentation (code does arithmetic and states
   the result), and question design. Never a hard-coded policy like
   "roam if the difference exceeds 6 dB".
3. **Validate cheaply, then confirm.** Presentation changes are first
   tried on real failure states (send variants of one recorded state and
   see how Jev's answer moves), then confirmed with a paired simulator
   batch against the classic arm.
4. **Log every round** in [docs/iterations.md](docs/iterations.md).

### Why the state is "described", not just dumped

TypeSafe documents that Jev 1.13 reads instructions literally, is weak at
arithmetic and numeric comparison, and loses accuracy when the state contains
irrelevant data ([jaggedness notes](https://docs.typesafe.ai/model-jaggedness/jev-1.13.md),
[state guide](https://docs.typesafe.ai/concepts/state.md)). So code does the
arithmetic — differences, trends, ages, MOS rating bands — and puts the result
next to the raw number. Code never applies a threshold that triggers an action;
that's Jev's job.

Jev is stateless, so the state carries memory: the last few events, and per-AP
history such as "last roam here 3 min ago: MOS 2.4 -> 4.4 (better)" or failed
attempts. Jev can see the consequences of its own earlier choices.

### Roam history and ping-pong

Jev is stateless and, per TypeSafe, doesn't count list items reliably, so
code counts and states the facts instead of handing over a raw roam log:

- per AP (candidates and the current AP): roams to it in the last 10 min,
  how long ago the client left it, average stay per visit;
- overall: roams in the last 10 min, and — when the latest changes alternate
  between the same two APs — a plain statement such as *"the client has
  switched back and forth between ap_a and ap_b 3 times in the last 2.1 min;
  average stay 24s"*.

None of this blocks anything (no hysteresis rail); it's information. Jev is
also asked a diagnostic noul, *"If the client roamed now, would that roam
likely be short-lived or reversed soon?"*, shown in the UI.

The scorecard counts **ping-pongs** (returning to the previous AP within
60 s, from the BSSID timeline, so external roams count too), average stay,
and roams per minute. To test it, `-sim-scenario boundary` parks the client
midway between two identical APs at about −76 dBm, where shadowing keeps
swapping which one looks stronger. `-hide-history` removes the history
facts so the two can be compared.

### Rails (the only places code overrides Jev)

| Rail | Default | Flag |
|---|---|---|
| Roam needs action confidence ≥ | 0.5 (TypeSafe's suggested "don't act below" line) | `-min-confidence` |
| Minimum gap between roams | 5 s | — |
| Minimum gap between scans | 4 s | — |
| Roam target must be a scanned AP, not the current one | always | — |
| Pause Jev after spending | $2.00 per run | `-budget` |

### Measuring link quality

- **MOS to the gateway.** Many gateways ignore ICMP, but every IPv4 gateway
  must answer ARP, so roamjev sends ARP requests to the gateway ~4×/s over a
  raw `AF_PACKET` socket and times the replies. Loss, latency and jitter feed
  the simplified ITU-T G.107 E-model to get a MOS (1.0–4.4). On one Wi-Fi hop
  latency is tiny, so loss and jitter dominate.
- **MCS / bitrate** from nl80211 station info (roamctl's netlink code).
- **Application retries:** TCP retransmitted ÷ sent segments from
  `/proc/net/snmp`. Host-wide (includes other interfaces) and only meaningful
  with traffic, so it's shown with the segment count.
- L2 retry counters aren't used; most drivers don't report them reliably.

Each roam is graded three ways:

- **Before/after:** MOS 15 s before vs 3–18 s after. It's all a real radio
  can measure, but it is optimistic: roams usually follow a dip, and MOS tends
  to recover after a dip anyway (regression to the mean), so almost every
  roam looks "better".
- **Baseline:** MOS 15–75 s before the roam, so you can see whether the roam
  beat normal conditions or merely ended a dip.
- **Vs staying (simulator only):** the simulator keeps generating probes for
  every AP as if the client were still associated there, so a roam is graded
  on actual MOS vs what staying would have given over the same window
  (including the roam's own disruption). This is the verdict the simulator's
  scorecard uses. Jev itself is only ever shown the before/after grade, since
  a real radio couldn't know the counterfactual.

`better` / `worse` means a difference above +0.1 / below −0.1 MOS.

## Dashboard

`http://127.0.0.1:8077` (localhost only). Everything is embedded in the binary.

- Live tiles: MOS, loss, latency, jitter, RSSI, MCS, TCP retransmits.
- Four synced timelines: link quality, signal/MCS, latency/jitter, and
  **what Jev is thinking** — its action probabilities stacked over time with
  the three diagnostic yes-probabilities overlaid. Scans and roams are marked
  on every chart.
- Decision inspector: click any moment to see the exact state sent, the
  questions, and every probability Jev returned, plus which rail (if any)
  blocked it.
- Candidate table as Jev saw it, scorecard, graded roam outcomes, full log.

## Running

```sh
mise exec go@1.25 -- go build -o roamjev ./cmd/roamjev

# 1. Simulator: a hallway of APs, real Jev calls, no radio involved.
./roamjev -sim                       # dashboard on :8077

# 2. Observe: Jev decides, nothing is executed.
sudo systemctl stop roamctl@wlp1s0
sudo ./roamjev -iface wlp1s0 -observe

# 3. Live: Jev drives the radio.
sudo ./roamjev -iface wlp1s0

# Review a past run (no radio, no API calls).
./roamjev -replay /var/lib/roamjev/<run>.jsonl

# Scorecards for one or more runs, side by side.
./roamjev -summary run1.jsonl run2.jsonl

# Ping-pong A/B in the simulator.
./roamjev -sim -sim-scenario boundary
./roamjev -sim -sim-scenario boundary -hide-history -listen 127.0.0.1:8078

# Check the gateway ARP probe alone (no Jev, no wpa_supplicant changes).
sudo ./roamjev -iface wlp1s0 -probe-test
```

API key lookup order: `$TYPESAFE_API_KEY`, `-key FILE`, `/etc/roamjev/api_key`,
`~/.config/roamctl-jev/api_key` (of `$SUDO_USER` when run with sudo).

Every run writes a JSONL journal (`/var/lib/roamjev/` as root, else
`~/.local/state/roamjev/`) with every tick, decision (full state + answers),
action, outcome and event.

## Cost

Jev costs $0.042 per million input tokens; output is free
([models](https://docs.typesafe.ai/models.md)). A decision with six candidates
is ~2,000 tokens ≈ $0.00008. At one decision every 3 s that's ≈ $2.40/day of
continuous running.
