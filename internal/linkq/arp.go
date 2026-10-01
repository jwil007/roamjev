//go:build linux

package linkq

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// ARPProber measures RTT to the default gateway with ARP requests. Every IPv4
// gateway must answer ARP, unlike ICMP echo, and the exchange crosses the
// Wi-Fi hop in both directions. It needs CAP_NET_RAW.
type ARPProber struct {
	Iface    string
	Interval time.Duration
	Timeout  time.Duration
	Ring     *Ring

	fd      int
	ifindex int
	mac     net.HardwareAddr
	ip      netip.Addr
	gw      netip.Addr
	gwMAC   net.HardwareAddr
	misses  int
	resolve time.Time
}

func NewARPProber(iface string, ring *Ring) *ARPProber {
	return &ARPProber{
		Iface:    iface,
		Interval: 250 * time.Millisecond,
		Timeout:  time.Second,
		Ring:     ring,
		fd:       -1,
	}
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

func (p *ARPProber) open() error {
	ifi, err := net.InterfaceByName(p.Iface)
	if err != nil {
		return fmt.Errorf("interface %s: %w", p.Iface, err)
	}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_DGRAM,
		int(htons(unix.ETH_P_ARP)))
	if err != nil {
		return fmt.Errorf("AF_PACKET socket (needs CAP_NET_RAW): %w", err)
	}
	sa := &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_ARP),
		Ifindex: ifi.Index}
	if err := unix.Bind(fd, sa); err != nil {
		_ = unix.Close(fd)
		return fmt.Errorf("bind AF_PACKET: %w", err)
	}
	p.fd = fd
	p.ifindex = ifi.Index
	p.mac = ifi.HardwareAddr
	return nil
}

// refresh re-reads our IPv4 address and the gateway, which can change after
// a roam to a different subnet or a DHCP renewal.
func (p *ARPProber) refresh() error {
	ifi, err := net.InterfaceByName(p.Iface)
	if err != nil {
		return err
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return err
	}
	p.ip = netip.Addr{}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if v4 := n.IP.To4(); v4 != nil {
				p.ip, _ = netip.AddrFromSlice(v4)
				break
			}
		}
	}
	gw, err := DefaultGateway(p.Iface)
	if err != nil {
		return err
	}
	if gw != p.gw {
		if p.gw.IsValid() {
			slog.Info("Gateway changed", "old", p.gw, "new", gw)
		}
		p.gw = gw
		p.gwMAC = nil
	}
	p.resolve = time.Now()
	return nil
}

// Gateway returns the probed gateway IP and MAC (MAC may be nil).
func (p *ARPProber) Gateway() (netip.Addr, net.HardwareAddr) {
	return p.gw, p.gwMAC
}

// Run probes until ctx is done.
func (p *ARPProber) Run(ctx context.Context) error {
	if err := p.open(); err != nil {
		return err
	}
	defer func() { _ = unix.Close(p.fd) }()
	go func() {
		<-ctx.Done()
		_ = unix.Shutdown(p.fd, unix.SHUT_RDWR)
	}()
	tcpTick := time.NewTicker(time.Second)
	defer tcpTick.Stop()
	next := time.Now()
	for ctx.Err() == nil {
		select {
		case <-tcpTick.C:
			if t, err := ReadTCPCounters(); err == nil {
				p.Ring.AddTCP(t)
			}
		default:
		}
		if time.Since(p.resolve) > 10*time.Second || !p.gw.IsValid() ||
			!p.ip.IsValid() {
			if err := p.refresh(); err != nil {
				slog.Debug("ARP probe: no gateway yet", "err", err)
				sleepCtx(ctx, time.Second)
				continue
			}
		}
		if !p.ip.IsValid() {
			sleepCtx(ctx, time.Second)
			continue
		}
		start := time.Now()
		rtt, err := p.probe()
		switch {
		case err == nil:
			p.misses = 0
			p.Ring.Add(Sample{At: start, RTT: rtt})
		case errors.Is(err, errTimeout):
			p.misses++
			// A unicast request to a stale MAC never gets answered.
			// Fall back to broadcast after a few misses.
			if p.misses >= 3 {
				p.gwMAC = nil
			}
			p.Ring.Add(Sample{At: start, Lost: true})
		default:
			if ctx.Err() != nil {
				return nil
			}
			// Interface down, mid-roam, etc. Count it as lost: the user's
			// traffic isn't getting through either.
			slog.Debug("ARP probe error", "err", err)
			p.Ring.Add(Sample{At: start, Lost: true})
			p.resolve = time.Time{}
		}
		next = next.Add(p.Interval)
		if d := time.Until(next); d > 0 {
			sleepCtx(ctx, d)
		} else {
			next = time.Now()
		}
	}
	return nil
}

var errTimeout = errors.New("arp timeout")

func (p *ARPProber) probe() (time.Duration, error) {
	pkt := make([]byte, 28)
	binary.BigEndian.PutUint16(pkt[0:], 1)      // HTYPE Ethernet
	binary.BigEndian.PutUint16(pkt[2:], 0x0800) // PTYPE IPv4
	pkt[4], pkt[5] = 6, 4
	binary.BigEndian.PutUint16(pkt[6:], 1) // request
	copy(pkt[8:14], p.mac)
	ip4 := p.ip.As4()
	copy(pkt[14:18], ip4[:])
	gw4 := p.gw.As4()
	copy(pkt[24:28], gw4[:])

	dst := net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	if p.gwMAC != nil {
		dst = p.gwMAC
	}
	sa := &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_ARP),
		Ifindex: p.ifindex, Halen: 6}
	copy(sa.Addr[:], dst)

	start := time.Now()
	if err := unix.Sendto(p.fd, pkt, 0, sa); err != nil {
		return 0, fmt.Errorf("sendto: %w", err)
	}
	deadline := start.Add(p.Timeout)
	buf := make([]byte, 128)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return 0, errTimeout
		}
		tv := unix.NsecToTimeval(left.Nanoseconds())
		if err := unix.SetsockoptTimeval(p.fd, unix.SOL_SOCKET,
			unix.SO_RCVTIMEO, &tv); err != nil {
			return 0, err
		}
		n, _, err := unix.Recvfrom(p.fd, buf, 0)
		if err != nil {
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
				continue
			}
			return 0, fmt.Errorf("recvfrom: %w", err)
		}
		if n < 28 || binary.BigEndian.Uint16(buf[6:]) != 2 {
			continue
		}
		if [4]byte(buf[14:18]) != gw4 || [4]byte(buf[24:28]) != ip4 {
			continue
		}
		rtt := time.Since(start)
		p.gwMAC = append(net.HardwareAddr(nil), buf[8:14]...)
		return rtt, nil
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// DefaultGateway reads the IPv4 default route for iface from /proc/net/route.
func DefaultGateway(iface string) (netip.Addr, error) {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return netip.Addr{}, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 || f[0] != iface || f[1] != "00000000" {
			continue
		}
		b, err := hex.DecodeString(f[2])
		if err != nil || len(b) != 4 {
			continue
		}
		// /proc/net/route stores addresses in host (little-endian) order.
		return netip.AddrFrom4([4]byte{b[3], b[2], b[1], b[0]}), nil
	}
	return netip.Addr{}, fmt.Errorf("no default route on %s", iface)
}

// ReadTCPCounters reads OutSegs and RetransSegs from /proc/net/snmp.
func ReadTCPCounters() (TCPSample, error) {
	b, err := os.ReadFile("/proc/net/snmp")
	if err != nil {
		return TCPSample{}, err
	}
	var hdr []string
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "Tcp:") {
			continue
		}
		f := strings.Fields(line)[1:]
		if hdr == nil {
			hdr = f
			continue
		}
		t := TCPSample{At: time.Now()}
		for i, name := range hdr {
			if i >= len(f) {
				break
			}
			v, _ := strconv.ParseUint(f[i], 10, 64)
			switch name {
			case "OutSegs":
				t.OutSegs = v
			case "RetransSegs":
				t.RetransSegs = v
			}
		}
		return t, nil
	}
	return TCPSample{}, errors.New("no Tcp line in /proc/net/snmp")
}
