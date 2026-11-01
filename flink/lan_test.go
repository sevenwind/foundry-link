package main

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"testing"
	"time"
)

func lanIP() netip.Addr {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() && ipn.IP.IsPrivate() {
			ip, _ := netip.AddrFromSlice(ipn.IP.To4())
			return ip
		}
	}
	return netip.Addr{}
}

// Режим без loopback: сокеты на всех интерфейсах (как в реальной работе), адрес — из локальной сети.
func newWildcardCore(t *testing.T, ip netip.Addr) *Core {
	c := newTestCore(t)
	c.opts.Local = false
	n, err := newNet(context.Background(), 0, false, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	f := n.family("v4")
	f.mapped = netip.AddrPortFrom(ip, f.conn.LocalAddr().(*net.UDPAddr).AddrPort().Port())
	f.nat = natCone
	c.net = n
	t.Cleanup(n.Close)
	return c
}

func TestWildcardLANStability(t *testing.T) {
	ip := lanIP()
	if !ip.IsValid() {
		t.Skip("нет адреса локальной сети")
	}
	fport := mockFoundry(t)
	host := newWildcardCore(t, ip)
	host.st.Update(func(s *Settings) { s.FoundryPort = fport })
	if err := host.StartHost(); err != nil {
		t.Fatal(err)
	}
	code := hostCode(t, host)
	var ps []*Core
	for i := 0; i < 3; i++ {
		p := newWildcardCore(t, ip)
		if err := p.StartPlayer(code, fmt.Sprintf("P%d", i+1)); err != nil {
			t.Fatal(err)
		}
		host.AddPlayers(replyCode(t, p))
		ps = append(ps, p)
	}
	waitFor(t, 20*time.Second, "3 игрока онлайн", func() bool { return host.State().Host.Online == 3 })
	for _, p := range ps {
		if err := echoCheck(p.State().Player.LocalPort, 8<<20); err != nil {
			t.Fatal(err)
		}
	}
	// Простой 30 секунд: связь не должна рваться, пинг должен идти
	time.Sleep(30 * time.Second)
	for i, p := range ps {
		v := p.State().Player
		if v.State != "online" || v.Reconnects != 0 || v.Stats.Loss > 0 {
			t.Errorf("игрок %d нестабилен: state=%s reconnects=%d loss=%.1f", i+1, v.State, v.Reconnects, v.Stats.Loss)
		}
		t.Logf("игрок %d: пинг %.1f мс, джиттер %.1f, потери %.1f%%, путь %s", i+1, v.Stats.Ping, v.Stats.Jitter, v.Stats.Loss, v.Path)
		if err := echoCheck(v.LocalPort, 2<<20); err != nil {
			t.Errorf("после простоя: %v", err)
		}
	}
}
