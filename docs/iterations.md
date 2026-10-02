# Iteration log

Paired simulator comparisons: 10 seeds per environment, same seed = same
world for both arms, 8-minute runs. "Jev − classic" with ~95% interval and
the number of pairs Jev won. Environments: office (light load, FT-PSK),
convention (dense, 55-95% load, PSK without FT), hospital (corridor,
802.1X without FT, voice badge), hallway (four dual-band APs).

| Round | State / change | Headline |
|---|---|---|
| cmp1 | v1: MOS-only goal, confidence rail 0.5 | Looked like a sticky client; later found the rail blocked 64 of 92 roam choices |
| cmp3 | v2: briefing, costs, environment, traffic; rail off | Jev wins voice (+0.17-0.25 MOS in 3 envs) and off-channel time (¼-½), loses throughput; drifts to 2.4 GHz |
| cmp4 | v2 + pre-roam check | Neutral overall; failed roams 1.1 → 0.3 in hospital; 22% of checks found a 6+ dB change |
| cmp5 | v3: objective metrics (no MOS/ratings), consistent rate estimates, band facts, noisier 2.4 GHz sim | Convention: voice +0.28, downloads tied, less 2.4 GHz than classic. Hallway: 5↔6 GHz flapping. Hospital: 2.4 GHz lock-in (candidate filter bias) |
| cmp6 | v4: band-balanced candidates and quick scan, "no roaming on marginal differences" fact, shared shadowing for co-located radios | running |
