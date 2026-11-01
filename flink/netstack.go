package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

type logFn func(format string, a ...any)

type natKind byte

const (
	natUnknown   natKind = iota // ответил один STUN-сервер
	natCone                     // внешний адрес не зависит от получателя
	natSymmetric                // для каждого получателя новый порт
	natUnstable                 // внешний IP меняется между серверами
	natBlocked                  // STUN недоступен
)

// Порядок важен: сначала серверы, отвечавшие и из РФ, и из других стран.
var stunServers = []string{
	"stun.cloudflare.com:3478",
	"stun.nextcloud.com:3478",
	"stun.sipgate.net:3478",
	"stun.m-online.net:3478",
	"stun.l.google.com:19302",
	"stun1.l.google.com:19302",
	"stun.voipgate.com:3478",
}

const (
	stunMagic   = 0x2112A442
	stunBinding = 0x0001
	stunSuccess = 0x0101
	attrMapped  = 0x0001
	attrXorMap  = 0x0020
)

// helloPkt — 12 байт с первым нулевым байтом: quic-go считает такой пакет «не QUIC»
// и отбрасывает. Нужен только чтобы открыть окно в NAT для встречного трафика.
var helloPkt = []byte{0x00, 'F', 'L', 'H', '1', 0, 0, 0, 0, 0, 0, 0}

// family — один UDP-сокет (IPv4 или IPv6). Через него идут и STUN, и QUIC,
// поэтому внешний порт, который видит STUN, совпадает с портом QUIC.
type family struct {
	name    string // "v4" | "v6"
	network string
	conn    *net.UDPConn
	tr      *quic.Transport

	mu      sync.Mutex
	mapped  netip.AddrPort
	nat     natKind
	waiters map[[12]byte]chan netip.AddrPort
	dns     map[string]netip.AddrPort
}

type Net struct {
	local bool
	fams  []*family
	log   logFn
	ctx   context.Context
	stop  context.CancelFunc
	wg    sync.WaitGroup // считыватели не-QUIC пакетов
}

func newNet(parent context.Context, port int, local bool, logf logFn) (*Net, error) {
	ctx, cancel := context.WithCancel(parent)
	n := &Net{local: local, log: logf, ctx: ctx, stop: cancel}
	for _, spec := range []struct{ name, network string }{{"v4", "udp4"}, {"v6", "udp6"}} {
		laddr := &net.UDPAddr{Port: port}
		if local {
			laddr = &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}
			if spec.name == "v6" {
				laddr = &net.UDPAddr{IP: net.IPv6loopback}
			}
		}
		conn, err := net.ListenUDP(spec.network, laddr)
		if err != nil && !local && port != 0 {
			conn, err = net.ListenUDP(spec.network, &net.UDPAddr{})
		}
		if err != nil {
			if spec.name == "v4" {
				cancel()
				return nil, err
			}
			logf("IPv6 недоступен: %v", err)
			continue
		}
		tuneUDP(conn)
		_ = conn.SetReadBuffer(4 << 20)
		_ = conn.SetWriteBuffer(4 << 20)
		f := &family{
			name: spec.name, network: spec.network, conn: conn,
			tr:      &quic.Transport{Conn: conn},
			waiters: map[[12]byte]chan netip.AddrPort{},
			dns:     map[string]netip.AddrPort{},
		}
		n.wg.Add(1)
		go func() { defer n.wg.Done(); f.readNonQUIC(ctx) }()
		n.fams = append(n.fams, f)
	}
	// Дать считывателям не-QUIC пакетов включиться до первого STUN-запроса.
	time.Sleep(100 * time.Millisecond)
	return n, nil
}

func (n *Net) Close() {
	n.stop()
	n.wg.Wait() // инициализация транспорта должна завершиться до его закрытия
	for _, f := range n.fams {
		_ = f.tr.Close()
		_ = f.conn.Close()
	}
}

func (n *Net) family(name string) *family {
	for _, f := range n.fams {
		if f.name == name {
			return f
		}
	}
	return nil
}

func (n *Net) familyFor(ap netip.AddrPort) *family {
	if ap.Addr().Is4() || ap.Addr().Is4In6() {
		return n.family("v4")
	}
	return n.family("v6")
}

func (n *Net) Endpoints() (v4, v6 netip.AddrPort) {
	if f := n.family("v4"); f != nil {
		f.mu.Lock()
		v4 = f.mapped
		f.mu.Unlock()
	}
	if f := n.family("v6"); f != nil {
		f.mu.Lock()
		v6 = f.mapped
		f.mu.Unlock()
	}
	return
}

// NAT возвращает тип NAT по IPv4. Если IPv4 нет, считаем, что связь идёт по IPv6 без NAT.
func (n *Net) NAT() natKind {
	if f := n.family("v4"); f != nil {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.nat
	}
	return natCone
}

func (n *Net) SendHello(ap netip.AddrPort) {
	if f := n.familyFor(ap); f != nil {
		_, _ = f.conn.WriteToUDPAddrPort(helloPkt, ap)
	}
}

// Refresh обновляет внешние адреса по всем семействам. Возвращает true, если что-то изменилось.
func (n *Net) Refresh(ctx context.Context) bool {
	var wg sync.WaitGroup
	var mu sync.Mutex
	changed := false
	for _, f := range n.fams {
		wg.Add(1)
		go func(f *family) {
			defer wg.Done()
			var c bool
			if n.local {
				c = f.setLocal()
			} else {
				c = f.refreshNAT(ctx)
			}
			if c {
				mu.Lock()
				changed = true
				mu.Unlock()
			}
		}(f)
	}
	wg.Wait()
	return changed
}

func (f *family) setLocal() bool {
	ap := f.conn.LocalAddr().(*net.UDPAddr).AddrPort()
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := f.mapped != ap
	f.mapped, f.nat = ap, natCone
	return ch
}

func (f *family) readNonQUIC(ctx context.Context) {
	buf := make([]byte, 1500)
	for {
		n, _, err := f.tr.ReadNonQUICPacket(ctx, buf)
		if err != nil {
			return
		}
		tid, ap, ok := parseSTUN(buf[:n])
		if !ok {
			continue
		}
		f.mu.Lock()
		ch := f.waiters[tid]
		f.mu.Unlock()
		if ch != nil {
			select {
			case ch <- ap:
			default:
			}
		}
	}
}

func (f *family) resolve(ctx context.Context, server string) (netip.AddrPort, error) {
	f.mu.Lock()
	if ap, ok := f.dns[server]; ok {
		f.mu.Unlock()
		return ap, nil
	}
	f.mu.Unlock()
	host, port, err := net.SplitHostPort(server)
	if err != nil {
		return netip.AddrPort{}, err
	}
	p, _ := net.LookupPort("udp", port)
	rctx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
	defer cancel()
	net4 := "ip4"
	if f.name == "v6" {
		net4 = "ip6"
	}
	ips, err := net.DefaultResolver.LookupNetIP(rctx, net4, host)
	if err != nil || len(ips) == 0 {
		return netip.AddrPort{}, errors.New("не резолвится")
	}
	ap := netip.AddrPortFrom(ips[0], uint16(p))
	f.mu.Lock()
	f.dns[server] = ap
	f.mu.Unlock()
	return ap, nil
}

func (f *family) stunQuery(ctx context.Context, server string) (netip.AddrPort, error) {
	dst, err := f.resolve(ctx, server)
	if err != nil {
		return netip.AddrPort{}, err
	}
	var tid [12]byte
	_, _ = rand.Read(tid[:])
	ch := make(chan netip.AddrPort, 1)
	f.mu.Lock()
	f.waiters[tid] = ch
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.waiters, tid)
		f.mu.Unlock()
	}()
	req := make([]byte, 20)
	binary.BigEndian.PutUint16(req[0:], stunBinding)
	binary.BigEndian.PutUint32(req[4:], stunMagic)
	copy(req[8:], tid[:])
	for i := 0; i < 2; i++ {
		_, _ = f.conn.WriteToUDPAddrPort(req, dst)
		select {
		case ap := <-ch:
			return ap, nil
		case <-time.After(1200 * time.Millisecond):
		case <-ctx.Done():
			return netip.AddrPort{}, ctx.Err()
		}
	}
	return netip.AddrPort{}, errors.New("нет ответа")
}

func (f *family) refreshNAT(ctx context.Context) bool {
	cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	type res struct {
		ap  netip.AddrPort
		err error
	}
	ch := make(chan res, len(stunServers))
	for _, s := range stunServers {
		go func(s string) {
			ap, err := f.stunQuery(cctx, s)
			ch <- res{ap, err}
		}(s)
	}
	var oks []netip.AddrPort
	for i := 0; i < len(stunServers); i++ {
		r := <-ch
		if r.err == nil {
			oks = append(oks, r.ap)
			if len(oks) >= 3 {
				break
			}
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	oldMapped, oldNAT := f.mapped, f.nat
	if len(oks) == 0 {
		if !f.mapped.IsValid() {
			f.nat = natBlocked
		}
		return f.nat != oldNAT
	}
	f.mapped = oks[0]
	f.nat = classify(oks)
	return f.mapped != oldMapped || f.nat != oldNAT
}

func classify(oks []netip.AddrPort) natKind {
	switch len(oks) {
	case 0:
		return natBlocked
	case 1:
		return natUnknown
	}
	sameIP, samePort := true, true
	for _, a := range oks[1:] {
		if a.Addr() != oks[0].Addr() {
			sameIP = false
		}
		if a.Port() != oks[0].Port() {
			samePort = false
		}
	}
	switch {
	case sameIP && samePort:
		return natCone
	case sameIP:
		return natSymmetric
	}
	return natUnstable
}

func parseSTUN(b []byte) (tid [12]byte, ap netip.AddrPort, ok bool) {
	if len(b) < 20 || binary.BigEndian.Uint16(b[0:]) != stunSuccess || binary.BigEndian.Uint32(b[4:]) != stunMagic {
		return
	}
	copy(tid[:], b[8:20])
	end := 20 + int(binary.BigEndian.Uint16(b[2:]))
	if end > len(b) {
		end = len(b)
	}
	var fallback netip.AddrPort
	for p := 20; p+4 <= end; {
		typ := binary.BigEndian.Uint16(b[p:])
		l := int(binary.BigEndian.Uint16(b[p+2:]))
		v := p + 4
		if v+l > end {
			break
		}
		switch typ {
		case attrXorMap:
			if a, ok2 := decodeAddr(b[v:v+l], true, tid); ok2 {
				return tid, a, true
			}
		case attrMapped:
			fallback, _ = decodeAddr(b[v:v+l], false, tid)
		}
		p = v + (l+3)&^3
	}
	return tid, fallback, fallback.IsValid()
}

func decodeAddr(v []byte, xor bool, tid [12]byte) (netip.AddrPort, bool) {
	if len(v) < 8 {
		return netip.AddrPort{}, false
	}
	port := binary.BigEndian.Uint16(v[2:])
	var mask [16]byte
	binary.BigEndian.PutUint32(mask[0:], stunMagic)
	copy(mask[4:], tid[:])
	if xor {
		port ^= uint16(stunMagic >> 16)
	}
	switch v[1] {
	case 1:
		var a [4]byte
		copy(a[:], v[4:8])
		if xor {
			for i := range a {
				a[i] ^= mask[i]
			}
		}
		return netip.AddrPortFrom(netip.AddrFrom4(a), port), true
	case 2:
		if len(v) < 20 {
			return netip.AddrPort{}, false
		}
		var a [16]byte
		copy(a[:], v[4:20])
		if xor {
			for i := range a {
				a[i] ^= mask[i]
			}
		}
		return netip.AddrPortFrom(netip.AddrFrom16(a), port), true
	}
	return netip.AddrPort{}, false
}

// natText — для интерфейса: уровень (good/warn/bad) и пояснение.
func natText(k natKind, hasV6 bool) (level, text string) {
	switch k {
	case natCone:
		return "good", "NAT конусный: прямое соединение пробивается хорошо"
	case natUnknown:
		return "warn", "Ответил один STUN-сервер: тип NAT пока не определён"
	case natSymmetric:
		if hasV6 {
			return "warn", "NAT симметричный: по IPv4 соединение возможно не у всех, но есть IPv6"
		}
		return "bad", "NAT симметричный: напрямую соединяются только с конусным NAT, нужен другой хост"
	case natUnstable:
		return "bad", "Внешний адрес меняется между серверами (CGNAT): P2P ненадёжен"
	}
	if hasV6 {
		return "warn", "IPv4 STUN недоступен, работает только IPv6"
	}
	return "bad", "STUN недоступен: возможно, заблокирован UDP, фаервол или VPN"
}
