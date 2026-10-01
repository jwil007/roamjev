//go:build linux

// roamjev hands Wi-Fi roaming and scanning decisions to Jev, TypeSafe's
// System One decision model, and shows what it does in a live dashboard.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	charmlog "charm.land/log/v2"
	"github.com/jwil007/roamjev/internal/agent"
	"github.com/jwil007/roamjev/internal/jev"
	"github.com/jwil007/roamjev/internal/linkq"
	"github.com/jwil007/roamjev/internal/sim"
	"github.com/jwil007/roamjev/internal/web"
	"github.com/jwil007/roamjev/internal/wpac"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("Fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	def := agent.DefaultConfig()
	iface := flag.String("iface", "wlan0", "Wi-Fi interface")
	simMode := flag.Bool("sim", false, "simulate a hallway of APs instead of using the radio")
	simSpeed := flag.Float64("sim-speed", 1.2, "simulated walking speed, m/s")
	replay := flag.String("replay", "", "serve the dashboard for a recorded journal (no radio, no Jev)")
	listen := flag.String("listen", "127.0.0.1:8077", "dashboard address")
	keyPath := flag.String("key", "", "API key file (default: $TYPESAFE_API_KEY, /etc/roamjev/api_key, ~/.config/roamctl-jev/api_key)")
	model := flag.String("model", jev.DefaultModel, "Jev model ID")
	journalDir := flag.String("journal-dir", "", "where run journals go (default /var/lib/roamjev as root, else ~/.local/state/roamjev)")
	interval := flag.Duration("interval", def.Interval, "time between decisions")
	jevTimeout := flag.Duration("jev-timeout", def.JevTimeout, "per-call timeout")
	minConf := flag.Float64("min-confidence", def.RoamMinConfidence, "roam only when Jev's action confidence is at least this")
	budget := flag.Float64("budget", def.BudgetUSD, "pause Jev calls after this many USD in one run (0 = no limit)")
	maxCands := flag.Int("candidates", def.MaxCandidates, "candidate APs shown to Jev")
	observe := flag.Bool("observe", false, "ask Jev but never act (wpa_supplicant keeps roaming normally)")
	allowBTM := flag.Bool("allow-btm", false, "leave 802.11v BTM enabled (lets APs steer the client)")
	level := flag.String("level", "info", "log level: debug, info")
	showVersion := flag.Bool("version", false, "print version")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}

	logger := charmlog.New(os.Stderr)
	if *level == "debug" {
		logger.SetLevel(charmlog.DebugLevel)
	}
	logger.SetReportTimestamp(true)
	logger.SetTimeFormat("15:04:05")
	slog.SetDefault(slog.New(logger))

	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if *replay != "" {
		store, err := agent.LoadJournal(*replay)
		if err != nil {
			return fmt.Errorf("load journal: %w", err)
		}
		srv := web.Serve(*listen, store)
		slog.Info("Replaying journal", "file", *replay,
			"dashboard", "http://"+*listen)
		<-ctx.Done()
		return srv.Close()
	}

	key, err := findKey(*keyPath)
	if err != nil {
		return err
	}
	jc := jev.New(key)
	jc.Model = *model

	cfg := def
	cfg.Interval = *interval
	cfg.JevTimeout = *jevTimeout
	cfg.RoamMinConfidence = *minConf
	cfg.BudgetUSD = *budget
	cfg.MaxCandidates = *maxCands
	cfg.Observe = *observe

	mode := "live"
	if *simMode {
		mode = "sim"
		*iface = "sim0"
	}
	if *observe {
		mode += "+observe"
	}
	journal, err := journalPath(*journalDir, mode, *iface)
	if err != nil {
		return err
	}
	store, err := agent.NewStore(journal)
	if err != nil {
		return err
	}
	defer store.Close()

	ring := linkq.NewRing(10 * time.Minute)
	var radio agent.Radio
	var gateway func() string
	if *simMode {
		w := sim.NewWorld(ring, *simSpeed)
		go w.Run(ctx)
		radio = w
		gateway = func() string { return fmt.Sprintf("simulated (client at x=%.0f m)", w.Position()) }
	} else {
		if pid, ok := roamctlRunning(*iface); ok {
			return fmt.Errorf("roamctl is running on %s (PID %d); stop it first: sudo systemctl stop roamctl@%s",
				*iface, pid, *iface)
		}
		c, err := wpac.Connect(*iface)
		if err != nil {
			return fmt.Errorf("connect to wpa_supplicant on %s: %w", *iface, err)
		}
		defer func() { _ = c.Close() }()
		radio = &agent.WPAS{C: c, AllowBTM: *allowBTM}
		prober := linkq.NewARPProber(*iface, ring)
		go func() {
			if err := prober.Run(ctx); err != nil {
				slog.Error("ARP prober stopped; MOS unavailable", "err", err)
				store.AddNote("probe", "ARP prober failed: "+err.Error())
			}
		}()
		gateway = func() string {
			ip, mac := prober.Gateway()
			if !ip.IsValid() {
				return "resolving"
			}
			if mac == nil {
				return ip.String()
			}
			return ip.String() + " (" + mac.String() + ")"
		}
	}

	store.SetInfo(agent.Info{Iface: *iface, Mode: mode, Model: jc.Model,
		Started: time.Now(), Interval: cfg.Interval.String(),
		BudgetUSD: cfg.BudgetUSD, Journal: journal, Version: version})
	srv := web.Serve(*listen, store)
	defer func() { _ = srv.Close() }()
	slog.Info("roamjev started", "mode", mode, "iface", *iface,
		"model", jc.Model, "dashboard", "http://"+*listen, "journal", journal)

	a := agent.New(cfg, radio, ring, jc, store)
	a.Gateway = gateway
	return a.Run(ctx)
}

func findKey(flagPath string) (string, error) {
	if flagPath != "" {
		return jev.LoadKey(flagPath)
	}
	paths := []string{"/etc/roamjev/api_key"}
	if su := os.Getenv("SUDO_USER"); su != "" {
		if u, err := user.Lookup(su); err == nil {
			paths = append(paths, filepath.Join(u.HomeDir, ".config/roamctl-jev/api_key"))
		}
	}
	if h, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(h, ".config/roamctl-jev/api_key"))
	}
	var lastErr error
	for _, p := range paths {
		k, err := jev.LoadKey(p)
		if err == nil {
			return k, nil
		}
		lastErr = err
	}
	return "", fmt.Errorf("no TypeSafe API key found (set TYPESAFE_API_KEY or -key): %w", lastErr)
}

func journalPath(dir, mode, iface string) (string, error) {
	if dir == "" {
		if os.Geteuid() == 0 {
			dir = "/var/lib/roamjev"
		} else {
			h, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			dir = filepath.Join(h, ".local/state/roamjev")
		}
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s-%s-%s.jsonl", time.Now().Format("20060102-150405"),
		strings.ReplaceAll(mode, "+", "-"), iface)
	return filepath.Join(dir, name), nil
}

func roamctlRunning(iface string) (int, bool) {
	b, err := os.ReadFile("/run/roamctl/" + iface + ".pid")
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, false
	}
	return pid, syscall.Kill(pid, 0) == nil
}
