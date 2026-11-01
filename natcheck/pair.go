package main

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"net"
	"strings"
	"time"
)

// ---- Коды ------------------------------------------------------------------
//
// Код = версия, роль, тип NAT, id сеанса (8 байт), IPv4:порт, контрольная сумма.
// Кодируется в base32 и группируется по 5 символов, чтобы его было удобно диктовать.

const (
	roleHost byte = 1
	roleJoin byte = 2
)

type pairCode struct {
	role byte
	nat  natKind
	sid  [8]byte
	ep   *net.UDPAddr
}

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func encodeCode(c pairCode) string {
	buf := []byte{1, c.role, byte(c.nat)}
	buf = append(buf, c.sid[:]...)
	buf = append(buf, c.ep.IP.To4()...)
	buf = binary.BigEndian.AppendUint16(buf, uint16(c.ep.Port))
	buf = binary.BigEndian.AppendUint16(buf, uint16(crc32.ChecksumIEEE(buf)))
	s := b32.EncodeToString(buf)
	var parts []string
	for len(s) > 5 {
		parts = append(parts, s[:5])
		s = s[5:]
	}
	parts = append(parts, s)
	return "NC1-" + strings.Join(parts, "-")
}

func decodeCode(s string) (pairCode, error) {
	var c pairCode
	s = strings.ToUpper(s)
	s = strings.NewReplacer(" ", "", "-", "", "\t", "", "\r", "", "\n", "").Replace(s)
	if !strings.HasPrefix(s, "NC1") {
		return c, errors.New("код должен начинаться с NC1-")
	}
	raw, err := b32.DecodeString(strings.TrimPrefix(s, "NC1"))
	if err != nil || len(raw) != 19 {
		return c, errors.New("код повреждён или скопирован не целиком")
	}
	if uint16(crc32.ChecksumIEEE(raw[:17])) != binary.BigEndian.Uint16(raw[17:]) {
		return c, errors.New("контрольная сумма не сходится: в коде опечатка")
	}
	if raw[0] != 1 {
		return c, errors.New("код от другой версии программы")
	}
	c.role = raw[1]
	c.nat = natKind(raw[2])
	copy(c.sid[:], raw[3:11])
	c.ep = &net.UDPAddr{IP: net.IP(append([]byte{}, raw[11:15]...)), Port: int(binary.BigEndian.Uint16(raw[15:17]))}
	return c, nil
}

// ---- Пакеты пробивки и измерения -----------------------------------------

const (
	pktHello byte = 1
	pktPing  byte = 2
	pktPong  byte = 3

	flagHeard byte = 1 // «я уже слышал вас»
)

var pktMagic = []byte("NCP1")

func mkPkt(typ, flags byte, sid [8]byte, seq uint32) []byte {
	b := append([]byte{}, pktMagic...)
	b = append(b, typ, flags)
	b = append(b, sid[:]...)
	return binary.BigEndian.AppendUint32(b, seq)
}

func parsePkt(b []byte, sid [8]byte) (typ, flags byte, seq uint32, ok bool) {
	if len(b) != 18 || string(b[:4]) != string(pktMagic) || string(b[6:14]) != string(sid[:]) {
		return
	}
	return b[4], b[5], binary.BigEndian.Uint32(b[14:]), true
}

// ---- Пробивка NAT --------------------------------------------------------

type punchResult struct {
	established bool
	heard       bool // от собеседника пришёл хотя бы один пакет
	observed    *net.UDPAddr
	elapsed     time.Duration
	aborted     bool
}

// punch шлёт hello на адрес собеседника и ждёт, пока обе стороны услышат друг друга.
// abort — канал ввода: любая введённая строка прекращает ожидание.
func punch(conn *net.UDPConn, peer *net.UDPAddr, sid [8]byte, timeout time.Duration, abort <-chan string) punchResult {
	var res punchResult
	target := peer
	peerHeard := false
	start := time.Now()
	deadline := start.Add(timeout)
	nextSend := start
	buf := make([]byte, 1500)

	hello := func(to *net.UDPAddr) {
		var f byte
		if res.heard {
			f = flagHeard
		}
		conn.WriteToUDP(mkPkt(pktHello, f, sid, 0), to)
	}

	for time.Now().Before(deadline) {
		if !time.Now().Before(nextSend) {
			hello(target)
			nextSend = time.Now().Add(250 * time.Millisecond)
		}
		select {
		case _, ok := <-abort:
			if ok {
				res.aborted = true
				res.elapsed = time.Since(start)
				return res
			}
			abort = nil
		default:
		}

		conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		typ, flags, _, ok := parsePkt(buf[:n], sid)
		if !ok {
			continue
		}
		first := !res.heard
		res.heard = true
		res.observed = src
		target = src
		if typ != pktHello || flags&flagHeard != 0 {
			peerHeard = true
		}
		if typ == pktHello && first {
			hello(src) // сразу сообщаем, что слышим
		}
		if res.heard && peerHeard {
			res.established = true
			break
		}
	}
	res.elapsed = time.Since(start)
	if res.established {
		for i := 0; i < 5; i++ { // чтобы собеседник тоже увидел «слышу»
			hello(target)
			time.Sleep(100 * time.Millisecond)
		}
	}
	return res
}

// ---- Измерение -----------------------------------------------------------

var plan = struct {
	quick, total int
	fast, slow   time.Duration
	tail, linger time.Duration
}{quick: 20, total: 46, fast: 200 * time.Millisecond, slow: time.Second, tail: 2 * time.Second, linger: 4 * time.Second}

type measureResult struct {
	sent, recv    int
	min, max, sum time.Duration
	maxGap        time.Duration
}

func measure(conn *net.UDPConn, target *net.UDPAddr, sid [8]byte) measureResult {
	var m measureResult
	sentAt := map[uint32]time.Time{}
	buf := make([]byte, 1500)
	start := time.Now()
	lastHeard := start
	nextSend := start
	var seq uint32
	sendDone := false
	var endAt time.Time

	handle := func(deadline time.Time) {
		conn.SetReadDeadline(deadline)
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		typ, _, sq, ok := parsePkt(buf[:n], sid)
		if !ok {
			return
		}
		t := time.Now()
		if g := t.Sub(lastHeard); g > m.maxGap {
			m.maxGap = g
		}
		lastHeard = t
		switch typ {
		case pktPing:
			conn.WriteToUDP(mkPkt(pktPong, flagHeard, sid, sq), src)
		case pktHello:
			conn.WriteToUDP(mkPkt(pktHello, flagHeard, sid, 0), src)
		case pktPong:
			if st, ok := sentAt[sq]; ok {
				rtt := t.Sub(st)
				delete(sentAt, sq)
				if m.recv == 0 || rtt < m.min {
					m.min = rtt
				}
				if rtt > m.max {
					m.max = rtt
				}
				m.sum += rtt
				m.recv++
			}
		}
	}

	for {
		now := time.Now()
		if !sendDone && !now.Before(nextSend) {
			seq++
			sentAt[seq] = now
			conn.WriteToUDP(mkPkt(pktPing, flagHeard, sid, seq), target)
			m.sent++
			switch {
			case m.sent >= plan.total:
				sendDone = true
				endAt = now.Add(plan.tail)
			case m.sent < plan.quick:
				nextSend = now.Add(plan.fast)
			default:
				nextSend = now.Add(plan.slow)
			}
		}
		if sendDone && now.After(endAt) {
			break
		}
		handle(now.Add(50 * time.Millisecond))
	}

	// Собеседник может ещё мерить: продолжаем отвечать на его пакеты.
	until := time.Now().Add(plan.linger)
	for time.Now().Before(until) {
		handle(time.Now().Add(50 * time.Millisecond))
	}
	return m
}

// ---- STUN для парного режима --------------------------------------------

// stunForPair спрашивает серверы по очереди с одного сокета, пока не получит три ответа.
func stunForPair(conn *net.UDPConn, servers []string) (natKind, *net.UDPAddr, *net.UDPAddr) {
	out("Определяю внешний адрес через STUN…")
	var oks []result
	var keep *net.UDPAddr
	for _, s := range servers {
		r := query(conn, "udp4", s)
		if !r.ok {
			out("  [FAIL] %s", s)
			continue
		}
		out("  [ OK ] %-28s → %s", s, r.mapped)
		oks = append(oks, r)
		if keep == nil {
			keep, _ = net.ResolveUDPAddr("udp4", s)
		}
		if len(oks) >= 3 {
			break
		}
	}
	kind, mapped := classify(oks)
	return kind, mapped, keep
}

// keepalive поддерживает «окно» в NAT, пока пользователь копирует код.
func keepalive(conn *net.UDPConn, stun *net.UDPAddr) (stop func()) {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(8 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				req := make([]byte, 20)
				binary.BigEndian.PutUint16(req[0:], bindingRequest)
				binary.BigEndian.PutUint32(req[4:], magicCookie)
				rand.Read(req[8:])
				conn.WriteToUDP(req, stun)
			}
		}
	}()
	return func() { close(done) }
}

// ---- Сценарии host / join -----------------------------------------------

func pairMode(isHost bool, extra []string) {
	title := "natcheck — парный тест (хост)"
	if !isHost {
		title = "natcheck — парный тест (второй участник)"
	}
	header(title)
	servers := append(append([]string{}, defaultServers...), extra...)

	var peer pairCode
	if !isHost {
		fmt.Print("Вставьте код хоста: ")
		code, err := decodeCode(readLine())
		if err != nil {
			out("✗ %v", err)
			writeReport("natcheck-pair-report.txt")
			return
		}
		if code.role != roleHost {
			out("✗ Это код второго участника, а нужен код хоста.")
			writeReport("natcheck-pair-report.txt")
			return
		}
		peer = code
		out("Код принят: хост %s (%s)", peer.ep, peer.nat)
	}

	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		out("✗ Не удалось открыть UDP-сокет: %v", err)
		writeReport("natcheck-pair-report.txt")
		return
	}
	defer conn.Close()

	nat, mapped, stunAddr := stunForPair(conn, servers)
	if mapped == nil {
		out("")
		out("✗ Ни один STUN-сервер не ответил: внешний адрес неизвестен, тест невозможен.")
		out("  Вероятно, исходящий UDP заблокирован (провайдер, фаервол, антивирус, VPN).")
		writeReport("natcheck-pair-report.txt")
		return
	}
	out("Ваш внешний адрес: %s (%s)", mapped, nat)
	out("")

	own := pairCode{role: roleHost, nat: nat, ep: mapped}
	if isHost {
		rand.Read(own.sid[:])
	} else {
		own.role = roleJoin
		own.sid = peer.sid
	}
	fmt.Println()
	if isHost {
		fmt.Println("Ваш код (отправьте второму участнику):")
	} else {
		fmt.Println("Ваш ответный код (отправьте хосту):")
	}
	fmt.Println("  " + encodeCode(own))
	fmt.Println()
	report.WriteString("Код этого участника: " + encodeCode(own) + "\n")

	var timeout time.Duration
	var abort <-chan string
	if isHost {
		stop := keepalive(conn, stunAddr)
		fmt.Print("Вставьте ответный код второго участника: ")
		code, err := decodeCode(readLine())
		stop()
		if err != nil {
			out("✗ %v", err)
			writeReport("natcheck-pair-report.txt")
			return
		}
		if code.role != roleJoin || code.sid != own.sid {
			out("✗ Это не ответный код на ваш текущий код (другой сеанс или роль).")
			writeReport("natcheck-pair-report.txt")
			return
		}
		peer = code
		out("Ответный код принят: участник %s (%s)", peer.ep, peer.nat)
		timeout = 20 * time.Second
	} else {
		fmt.Println("Жду соединения с хостом (до 2 минут; Enter — прекратить ожидание)…")
		timeout = 2 * time.Minute
	}
	abort = lines

	out("")
	out("Пробиваю соединение…")
	pr := punch(conn, peer.ep, own.sid, timeout, abort)

	out("")
	out("=== Результат ===")
	if !pr.established {
		reportFailure(pr, nat, peer, mapped, isHost)
		writeReport("natcheck-pair-report.txt")
		return
	}

	if isHost {
		out("✓ Прямое соединение установлено за %.1f с", pr.elapsed.Seconds())
	} else {
		out("✓ Прямое соединение установлено")
	}
	out("  Вы:          %s (%s)", mapped, nat)
	extraNote := ""
	if pr.observed != nil && !pr.observed.IP.Equal(peer.ep.IP) || pr.observed != nil && pr.observed.Port != peer.ep.Port {
		extraNote = fmt.Sprintf(" — пакеты пришли с %s, а не с адреса из кода (признак симметричного NAT)", pr.observed)
	}
	out("  Собеседник:  %s (%s)%s", peer.ep, peer.nat, extraNote)
	out("")
	out("Измеряю пинг и стабильность (около 35 секунд, не закрывайте окно)…")

	m := measure(conn, pr.observed, own.sid)
	lost := m.sent - m.recv
	out("")
	if m.recv == 0 {
		out("✗ Рукопожатие прошло, но ни один пакет измерения не вернулся (0 из %d).", m.sent)
		out("  Связь односторонняя или нестабильная: попробуйте повторить тест.")
		writeReport("natcheck-pair-report.txt")
		return
	}
	avg := m.sum / time.Duration(m.recv)
	out("Пинг (%d замеров): мин %d мс · средн %d мс · макс %d мс", m.recv, m.min.Milliseconds(), avg.Milliseconds(), m.max.Milliseconds())
	out("Потери пакетов: %d из %d", lost, m.sent)
	if m.maxGap < 3*time.Second {
		out("Стабильность: связь не прерывалась (максимальная пауза %.1f с)", m.maxGap.Seconds())
	} else {
		out("Стабильность: были паузы до %.1f с", m.maxGap.Seconds())
	}
	out("")
	switch {
	case lost == 0 && m.maxGap < 3*time.Second:
		out("Вывод: P2P между вами будет работать хорошо.")
	case lost*10 <= m.sent:
		out("Вывод: P2P работает, но есть потери пакетов; для игры допустимо, стоит перепроверить в другое время.")
	default:
		out("Вывод: соединение установилось, но нестабильно (много потерь).")
	}
	writeReport("natcheck-pair-report.txt")
}

func reportFailure(pr punchResult, own natKind, peer pairCode, mapped *net.UDPAddr, isHost bool) {
	switch {
	case pr.aborted:
		out("✗ Ожидание прервано вручную.")
	case pr.heard:
		out("✗ Пакеты от собеседника доходят, но он не получает ваши (соединение только в одну сторону).")
	default:
		out("✗ Прямое соединение не установлено (от собеседника не пришло ни одного пакета).")
	}
	out("  Вы:          %s (%s)", mapped, own)
	out("  Собеседник:  %s (%s)", peer.ep, peer.nat)
	out("")
	switch {
	case own == natSymmetric && peer.nat == natSymmetric:
		out("Причина: у обоих симметричный NAT, напрямую такие сети не соединяются.")
	case own == natSymmetric || peer.nat == natSymmetric:
		out("Вероятная причина: симметричный NAT у одной из сторон (для каждого адреса выдаётся новый порт).")
	case own == natUnstable || peer.nat == natUnstable:
		out("Вероятная причина: у одной из сторон внешний адрес меняется между соединениями (CGNAT).")
	case own == natUnknown || peer.nat == natUnknown:
		out("Тип NAT у одной из сторон не определён (ответил один STUN-сервер), причина неясна.")
	default:
		out("У обеих сторон NAT конусный, поэтому причина, скорее всего, не в нём: проверьте")
		out("фаервол и антивирус, VPN, и что второй участник вставил код вовремя (не позже пары минут).")
	}
	out("Что можно попробовать: другая сеть (проводной интернет вместо мобильного), поменяться ролями")
	out("хоста и участника, повторить тест, если код вставляли с задержкой.")
}
