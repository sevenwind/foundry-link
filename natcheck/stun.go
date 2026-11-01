package main

import (
	"crypto/rand"
	"encoding/binary"
	"net"
	"time"
)

// Порядок важен: сначала серверы, отвечавшие и из РФ, и из других стран.
var defaultServers = []string{
	"stun.cloudflare.com:3478",
	"stun.nextcloud.com:3478",
	"stun.sipgate.net:3478",
	"stun.m-online.net:3478",
	"stun.l.google.com:19302",
	"stun1.l.google.com:19302",
	"stun.voipgate.com:3478",
}

const (
	magicCookie    = 0x2112A442
	attrMapped     = 0x0001
	attrXorMapped  = 0x0020
	bindingRequest = 0x0001
	bindingSuccess = 0x0101
	attemptTimeout = 2 * time.Second
	attempts       = 2
)

type result struct {
	server  string
	ok      bool
	mapped  *net.UDPAddr
	rtt     time.Duration
	problem string
}

type natKind byte

const (
	natUnknown   natKind = iota // ответил один сервер
	natCone                     // внешний адрес не зависит от получателя
	natSymmetric                // для каждого получателя новый порт
	natUnstable                 // внешний IP меняется между серверами
	natBlocked                  // STUN недоступен
)

func (k natKind) String() string {
	switch k {
	case natCone:
		return "конусный NAT"
	case natSymmetric:
		return "СИММЕТРИЧНЫЙ NAT"
	case natUnstable:
		return "нестабильный адрес (CGNAT / пул адресов)"
	case natBlocked:
		return "STUN недоступен"
	}
	return "тип NAT не определён"
}

func stunMode(extra []string) {
	servers := append(append([]string{}, defaultServers...), extra...)

	header("natcheck — проверка STUN и типа NAT")
	v4 := probe("udp4", servers)
	v6 := probe("udp6", servers)

	out("")
	out("=== Итог ===")
	analyze("IPv4", v4)
	analyzeV6(v6)

	writeReport("natcheck-report.txt")
}

func probe(network string, servers []string) []result {
	label := "IPv4"
	if network == "udp6" {
		label = "IPv6"
	}
	out("=== %s: запросы к STUN-серверам ===", label)

	conn, err := net.ListenUDP(network, nil)
	if err != nil {
		out("  не удалось открыть UDP-сокет (%v) — %s недоступен", err, label)
		return nil
	}
	defer conn.Close()
	local := conn.LocalAddr().(*net.UDPAddr)
	out("  локальный порт сокета: %d (один и тот же для всех запросов)", local.Port)

	var res []result
	for _, s := range servers {
		r := query(conn, network, s)
		res = append(res, r)
		if r.ok {
			out("  [ OK ] %-28s → %-22s %4d мс", s, r.mapped, r.rtt.Milliseconds())
		} else {
			out("  [FAIL] %-28s %s", s, r.problem)
		}
	}
	return res
}

func query(conn *net.UDPConn, network, server string) result {
	r := result{server: server}
	addr, err := net.ResolveUDPAddr(network, server)
	if err != nil {
		r.problem = "имя не резолвится в адрес " + map[string]string{"udp4": "IPv4", "udp6": "IPv6"}[network] + " (DNS заблокирован или адреса нет)"
		return r
	}
	for i := 0; i < attempts; i++ {
		var tid [12]byte
		rand.Read(tid[:])
		req := make([]byte, 20)
		binary.BigEndian.PutUint16(req[0:], bindingRequest)
		binary.BigEndian.PutUint32(req[4:], magicCookie)
		copy(req[8:], tid[:])

		start := time.Now()
		if _, err := conn.WriteToUDP(req, addr); err != nil {
			r.problem = "ошибка отправки: " + err.Error()
			return r
		}
		conn.SetReadDeadline(time.Now().Add(attemptTimeout))
		buf := make([]byte, 1500)
		for {
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				break // таймаут — следующая попытка
			}
			if m := parse(buf[:n], tid); m != nil {
				r.ok, r.mapped, r.rtt = true, m, time.Since(start)
				return r
			}
		}
	}
	r.problem = "нет ответа (UDP к серверу заблокирован или сервер не работает)"
	return r
}

func parse(b []byte, tid [12]byte) *net.UDPAddr {
	if len(b) < 20 || binary.BigEndian.Uint16(b[0:]) != bindingSuccess {
		return nil
	}
	if binary.BigEndian.Uint32(b[4:]) != magicCookie || string(b[8:20]) != string(tid[:]) {
		return nil
	}
	end := 20 + int(binary.BigEndian.Uint16(b[2:]))
	if end > len(b) {
		end = len(b)
	}
	var fallback *net.UDPAddr
	for p := 20; p+4 <= end; {
		typ := binary.BigEndian.Uint16(b[p:])
		l := int(binary.BigEndian.Uint16(b[p+2:]))
		v := p + 4
		if v+l > end {
			break
		}
		val := b[v : v+l]
		switch typ {
		case attrXorMapped:
			if a := decode(val, true, tid); a != nil {
				return a
			}
		case attrMapped:
			fallback = decode(val, false, tid)
		}
		p = v + (l+3)&^3
	}
	return fallback
}

func decode(v []byte, xor bool, tid [12]byte) *net.UDPAddr {
	if len(v) < 8 {
		return nil
	}
	port := int(binary.BigEndian.Uint16(v[2:]))
	var ip net.IP
	switch v[1] {
	case 1:
		ip = net.IP(append([]byte{}, v[4:8]...))
	case 2:
		if len(v) < 20 {
			return nil
		}
		ip = net.IP(append([]byte{}, v[4:20]...))
	default:
		return nil
	}
	if xor {
		port ^= magicCookie >> 16
		var mask [16]byte
		binary.BigEndian.PutUint32(mask[0:], magicCookie)
		copy(mask[4:], tid[:])
		for i := range ip {
			ip[i] ^= mask[i]
		}
	}
	return &net.UDPAddr{IP: ip, Port: port}
}

// classify определяет тип NAT по ответам нескольких серверов на один и тот же сокет.
func classify(rs []result) (natKind, *net.UDPAddr) {
	var ok []result
	for _, r := range rs {
		if r.ok {
			ok = append(ok, r)
		}
	}
	if len(ok) == 0 {
		return natBlocked, nil
	}
	first := ok[0].mapped
	if len(ok) < 2 {
		return natUnknown, first
	}
	sameIP, samePort := true, true
	for _, r := range ok[1:] {
		if !r.mapped.IP.Equal(first.IP) {
			sameIP = false
		}
		if r.mapped.Port != first.Port {
			samePort = false
		}
	}
	switch {
	case sameIP && samePort:
		return natCone, first
	case sameIP:
		return natSymmetric, first
	}
	return natUnstable, first
}

func analyze(label string, rs []result) {
	n := 0
	for _, r := range rs {
		if r.ok {
			n++
		}
	}
	out("%s: ответили %d из %d серверов", label, n, len(rs))
	kind, first := classify(rs)
	switch kind {
	case natBlocked:
		out("  ✗ Ни один STUN-сервер не ответил. Вероятно, исходящий UDP заблокирован")
		out("    (провайдер, фаервол, антивирус или VPN). P2P в таком виде работать не будет.")
		return
	case natUnknown:
		out("  Ваш внешний адрес: %s", first)
		out("  ? Ответил только один сервер — тип NAT определить нельзя. Запустите ещё раз или добавьте серверы аргументами.")
		return
	}

	out("  Ваш внешний адрес: %s", first)
	switch kind {
	case natCone:
		out("  ✓ NAT «конусный» (адрес не зависит от получателя). Прямое P2P-соединение пробивается хорошо.")
	case natSymmetric:
		out("  ✗ NAT СИММЕТРИЧНЫЙ (для каждого получателя новый порт). Пробивка сработает только если")
		out("    у второго участника NAT конусный; два симметричных NAT напрямую не соединятся.")
	default:
		out("  ✗ Внешний IP меняется между серверами (пул адресов провайдера / CGNAT). P2P ненадёжен.")
	}

	if isCGNAT(first.IP) {
		out("  ✗ Внешний адрес %s из диапазона CGNAT (100.64.0.0/10): провайдер сам раздаёт серые адреса.", first.IP)
	} else if first.IP.IsPrivate() {
		out("  ✗ Внешний адрес частный (%s): перед вами ещё один NAT (двойной NAT / CGNAT).", first.IP)
	}
	if hasLocalIP(first.IP) {
		out("  ✓ Внешний адрес совпадает с адресом на этом компьютере: NAT нет, вы напрямую в интернете.")
	}
}

func analyzeV6(rs []result) {
	for _, r := range rs {
		if r.ok {
			out("IPv6: работает, ваш адрес %s. Если он есть и у второго участника, соединение будет прямым и без NAT.", r.mapped.IP)
			return
		}
	}
	out("IPv6: недоступен (это нормально, у многих его нет).")
}

func isCGNAT(ip net.IP) bool {
	_, cg, _ := net.ParseCIDR("100.64.0.0/10")
	return cg.Contains(ip)
}

func hasLocalIP(ip net.IP) bool {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.Equal(ip) {
			return true
		}
	}
	return false
}
