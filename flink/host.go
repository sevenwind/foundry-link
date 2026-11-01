package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
)

type hostPlayer struct {
	mu           sync.Mutex
	rec          PlayerRec
	fp           [16]byte
	conn         quic.Connection
	stats        *Stats
	sent, recv   atomic.Int64
	since        time.Time
	reconnects   int
	everOnline   bool
	path         string
	ep4, ep6     netip.AddrPort
	offlineSince time.Time
}

type Host struct {
	c         *Core
	net       *Net
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	players   []*hostPlayer
	foundryUp atomic.Bool
}

func newHostPlayer(rec PlayerRec) (*hostPlayer, error) {
	raw, err := hex.DecodeString(rec.ID)
	if err != nil || len(raw) != 16 {
		return nil, errors.New("неверный ID игрока")
	}
	p := &hostPlayer{rec: rec, stats: newStats(), offlineSince: time.Now()}
	copy(p.fp[:], raw)
	if ap, err := netip.ParseAddrPort(rec.V4); err == nil {
		p.ep4 = ap
	}
	if ap, err := netip.ParseAddrPort(rec.V6); err == nil {
		p.ep6 = ap
	}
	return p, nil
}

func (c *Core) StartHost() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.host != nil {
		return nil
	}
	if c.player != nil {
		c.player.stop()
		c.player = nil
	}
	n, err := c.ensureNetLocked()
	if err != nil {
		return err
	}
	h := &Host{c: c, net: n}
	h.ctx, h.cancel = context.WithCancel(c.ctx)
	for _, rec := range c.st.Get().Players {
		if p, err := newHostPlayer(rec); err == nil {
			h.players = append(h.players, p)
		}
	}
	listening := 0
	for _, f := range n.fams {
		ln, err := f.tr.Listen(h.tlsConf(), quicConf())
		if err != nil {
			c.logf("Не удалось слушать %s: %v", f.name, err)
			continue
		}
		listening++
		go h.acceptLoop(ln, f.name)
	}
	if listening == 0 {
		h.cancel()
		c.releaseNetLocked()
		return errors.New("не удалось начать приём подключений")
	}
	go h.punchLoop()
	go h.foundryLoop()
	c.host = h
	c.st.Update(func(s *Settings) { s.Mode, s.HostEnabled = "host", true })
	c.logf("Хост запущен, игроков в списке: %d", len(h.players))
	return nil
}

func (c *Core) StopHost() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.host != nil {
		c.host.stop()
		c.host = nil
		c.releaseNetLocked()
		c.logf("Хост остановлен")
	}
	c.st.Update(func(s *Settings) { s.HostEnabled = false })
}

func (h *Host) stop() {
	h.cancel()
	h.mu.Lock()
	for _, p := range h.players {
		p.mu.Lock()
		if p.conn != nil {
			p.conn.CloseWithError(10, "host stopped")
		}
		p.mu.Unlock()
	}
	h.mu.Unlock()
}

func (h *Host) tlsConf() *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{h.c.id.Cert},
		MinVersion:   tls.VersionTLS13,
		NextProtos:   []string{alpn},
		ClientAuth:   tls.RequireAnyClientCert,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			fp, err := firstCertFP(raw)
			if err != nil {
				return err
			}
			if h.player(fp) == nil {
				return errors.New("игрок не допущен")
			}
			return nil
		},
	}
}

func (h *Host) player(fp [16]byte) *hostPlayer {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range h.players {
		if p.fp == fp {
			return p
		}
	}
	return nil
}

// ---- Управление списком игроков -----------------------------------------

// AddPlayers принимает один или несколько ответных кодов. Вставка кода и есть допуск игрока.
func (c *Core) AddPlayers(text string) (added int, errs []string) {
	c.mu.Lock()
	h := c.host
	c.mu.Unlock()
	if h == nil {
		return 0, []string{"Хост не запущен"}
	}
	found := false
	for _, tok := range strings.Fields(text) {
		if !strings.HasPrefix(strings.ToUpper(tok), "FL1") {
			continue
		}
		found = true
		code, err := DecodeCode(tok)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		if err := h.addPlayer(code); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		added++
	}
	if !found {
		errs = append(errs, "Не нашёл код: он начинается с FL1-")
	}
	return
}

func (h *Host) addPlayer(code Code) error {
	if code.Role != rolePlayer {
		return errors.New("Это код комнаты, а нужен ответный код игрока")
	}
	if code.FP == h.c.id.FP {
		return errors.New("Это ваш собственный код")
	}
	h.mu.Lock()
	var p *hostPlayer
	for _, x := range h.players {
		if x.fp == code.FP {
			p = x
		}
	}
	if p == nil {
		if len(h.players) >= maxPlayers {
			h.mu.Unlock()
			return fmt.Errorf("Достигнут предел: %d игроков. Удалите кого-нибудь из списка", maxPlayers)
		}
		p = &hostPlayer{fp: code.FP, stats: newStats(), offlineSince: time.Now()}
		p.rec.ID = hex.EncodeToString(code.FP[:])
		p.rec.Added = time.Now().Unix()
		h.players = append(h.players, p)
	}
	h.mu.Unlock()

	p.mu.Lock()
	if n := cleanName(code.Name); n != "" {
		p.rec.Name = n
	}
	if code.V4.IsValid() {
		p.ep4, p.rec.V4 = code.V4, code.V4.String()
	}
	if code.V6.IsValid() {
		p.ep6, p.rec.V6 = code.V6, code.V6.String()
	}
	eps := []netip.AddrPort{p.ep4, p.ep6}
	p.mu.Unlock()
	h.persist()
	for _, ep := range eps {
		if ep.IsValid() {
			h.net.SendHello(ep)
		}
	}
	h.c.logf("Игрок допущен: %s", displayName(p))
	return nil
}

func (c *Core) RemovePlayer(id string) {
	c.mu.Lock()
	h := c.host
	c.mu.Unlock()
	if h == nil {
		return
	}
	h.mu.Lock()
	var removed *hostPlayer
	for i, p := range h.players {
		if p.rec.ID == id {
			removed = p
			h.players = append(h.players[:i], h.players[i+1:]...)
			break
		}
	}
	h.mu.Unlock()
	if removed == nil {
		return
	}
	removed.mu.Lock()
	if removed.conn != nil {
		removed.conn.CloseWithError(11, "removed")
	}
	removed.mu.Unlock()
	h.persist()
	h.c.logf("Игрок удалён: %s", displayName(removed))
}

func (h *Host) persist() {
	h.mu.Lock()
	recs := make([]PlayerRec, 0, len(h.players))
	for _, p := range h.players {
		p.mu.Lock()
		recs = append(recs, p.rec)
		p.mu.Unlock()
	}
	h.mu.Unlock()
	h.c.st.Update(func(s *Settings) { s.Players = recs })
}

func displayName(p *hostPlayer) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rec.Name != "" {
		return p.rec.Name
	}
	return "Игрок " + shortID(p.rec.ID)
}

func (c *Core) SetHostSettings(name string, foundryPort int) error {
	if foundryPort < 1 || foundryPort > 65535 {
		return errors.New("порт должен быть от 1 до 65535")
	}
	c.st.Update(func(s *Settings) {
		if strings.TrimSpace(name) != "" {
			s.HostName = strings.TrimSpace(name)
		}
		s.FoundryPort = foundryPort
	})
	c.changed()
	return nil
}

// ---- Приём подключений ---------------------------------------------------

func (h *Host) acceptLoop(ln *quic.Listener, fam string) {
	for {
		conn, err := ln.Accept(h.ctx)
		if err != nil {
			return
		}
		go h.handleConn(conn, fam)
	}
}

func (h *Host) handleConn(conn quic.Connection, fam string) {
	certs := conn.ConnectionState().TLS.PeerCertificates
	if len(certs) == 0 {
		conn.CloseWithError(1, "no certificate")
		return
	}
	p := h.player(fpOf(certs[0].Raw))
	if p == nil {
		conn.CloseWithError(2, "not allowed")
		return
	}
	actx, cancel := context.WithTimeout(conn.Context(), 8*time.Second)
	cs, err := conn.AcceptStream(actx)
	cancel()
	if err != nil {
		conn.CloseWithError(3, "no control stream")
		return
	}
	_ = cs.SetReadDeadline(time.Now().Add(6 * time.Second))
	var kind [1]byte
	if _, err := io.ReadFull(cs, kind[:]); err != nil || kind[0] != kindControl {
		conn.CloseWithError(3, "bad control stream")
		return
	}
	hello, err := readMsg(cs)
	if err != nil || hello.T != "hello" {
		conn.CloseWithError(3, "bad hello")
		return
	}
	_ = cs.SetReadDeadline(time.Time{})
	if p2 := h.player(p.fp); p2 == nil { // удалили, пока шло рукопожатие
		_ = writeMsg(cs, nil, ctrlMsg{T: "deny", Reason: "removed"})
		conn.CloseWithError(2, "removed")
		return
	}
	hostName := h.c.st.Get().HostName
	if err := writeMsg(cs, nil, ctrlMsg{T: "welcome", Host: hostName, Max: maxPlayers}); err != nil {
		conn.CloseWithError(3, "welcome failed")
		return
	}

	// Новое подключение заменяет старое (например, после обрыва, о котором хост ещё не знает).
	p.mu.Lock()
	old := p.conn
	p.conn = conn
	p.stats.reset()
	p.since = time.Now()
	if p.everOnline {
		p.reconnects++
	}
	p.everOnline = true
	if n := cleanName(hello.Name); n != "" {
		p.rec.Name = n
	}
	ap := remoteAP(conn)
	if ap.Addr().Is4() {
		p.ep4, p.rec.V4, p.path = ap, ap.String(), "IPv4"
	} else {
		p.ep6, p.rec.V6, p.path = ap, ap.String(), "IPv6"
	}
	p.mu.Unlock()
	if old != nil {
		old.CloseWithError(4, "replaced")
	}
	h.persist()
	h.c.logf("%s подключился (%s)", displayName(p), fam)

	go runPinger(conn.Context(), conn, p.stats, func() { conn.CloseWithError(5, "watchdog") })
	go func() { // управляющий поток живёт, пока живо соединение
		for {
			if _, err := readMsg(cs); err != nil {
				conn.CloseWithError(0, "control closed")
				return
			}
		}
	}()
	go func() {
		<-conn.Context().Done()
		p.mu.Lock()
		if p.conn == conn {
			p.conn = nil
			p.offlineSince = time.Now()
		}
		p.mu.Unlock()
		h.c.logf("%s отключился", displayName(p))
	}()

	for {
		s, err := conn.AcceptStream(conn.Context())
		if err != nil {
			return
		}
		go h.serveStream(p, s)
	}
}

func remoteAP(conn quic.Connection) netip.AddrPort {
	if u, ok := conn.RemoteAddr().(*net.UDPAddr); ok {
		ap := u.AddrPort()
		return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
	}
	return netip.AddrPort{}
}

func (h *Host) serveStream(p *hostPlayer, s quic.Stream) {
	var kind [1]byte
	if _, err := io.ReadFull(s, kind[:]); err != nil || kind[0] != kindTCP {
		s.CancelRead(1)
		s.CancelWrite(1)
		return
	}
	port := h.c.st.Get().FoundryPort
	c, err := net.DialTimeout("tcp", net.JoinHostPort("localhost", strconv.Itoa(port)), 3*time.Second)
	if err != nil {
		s.CancelRead(2)
		s.CancelWrite(2)
		return
	}
	pipe(c.(*net.TCPConn), s, &p.sent, &p.recv)
}

// punchLoop: пока игрок не на связи, шлём ему «стук» в NAT, чтобы его пакеты проходили к нам.
func (h *Host) punchLoop() {
	t := time.NewTicker(700 * time.Millisecond)
	defer t.Stop()
	n := 0
	for {
		select {
		case <-h.ctx.Done():
			return
		case <-t.C:
		}
		n++
		h.mu.Lock()
		ps := append([]*hostPlayer(nil), h.players...)
		h.mu.Unlock()
		for _, p := range ps {
			p.mu.Lock()
			offline := p.conn == nil
			age := time.Since(p.offlineSince)
			eps := []netip.AddrPort{p.ep4, p.ep6}
			p.mu.Unlock()
			if !offline || (age > 2*time.Minute && n%4 != 0) {
				continue
			}
			for _, ep := range eps {
				if ep.IsValid() {
					h.net.SendHello(ep)
				}
			}
		}
	}
}

func (h *Host) foundryLoop() {
	for {
		port := h.c.st.Get().FoundryPort
		c, err := net.DialTimeout("tcp", net.JoinHostPort("localhost", strconv.Itoa(port)), 500*time.Millisecond)
		up := err == nil
		if up {
			c.Close()
		}
		if h.foundryUp.Swap(up) != up {
			h.c.changed()
		}
		select {
		case <-h.ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// ---- Представление для интерфейса ---------------------------------------

type PlayerRow struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Short      string    `json:"short"`
	State      string    `json:"state"` // online | waiting
	Path       string    `json:"path"`
	Endpoint   string    `json:"endpoint"`
	Stats      StatsView `json:"stats"`
	Sent       int64     `json:"sent"`
	Recv       int64     `json:"recv"`
	Since      int64     `json:"since"`
	Reconnects int       `json:"reconnects"`
}

type HostView struct {
	Running   bool        `json:"running"`
	Code      string      `json:"code"`
	FoundryUp bool        `json:"foundryUp"`
	Online    int         `json:"online"`
	Players   []PlayerRow `json:"players"`
}

func (h *Host) View() *HostView {
	v4, v6 := h.net.Endpoints()
	v := &HostView{Running: true, FoundryUp: h.foundryUp.Load(), Players: []PlayerRow{}}
	if v4.IsValid() || v6.IsValid() {
		v.Code = Code{Role: roleHost, NAT: h.net.NAT(), FP: h.c.id.FP, V4: v4, V6: v6}.Encode()
	}
	h.mu.Lock()
	ps := append([]*hostPlayer(nil), h.players...)
	h.mu.Unlock()
	for _, p := range ps {
		p.mu.Lock()
		row := PlayerRow{ID: p.rec.ID, Name: p.rec.Name, Short: shortID(p.rec.ID), State: "waiting",
			Path: p.path, Sent: p.sent.Load(), Recv: p.recv.Load(), Reconnects: p.reconnects}
		if row.Name == "" {
			row.Name = "Игрок " + row.Short
		}
		if p.conn != nil {
			row.State = "online"
			row.Since = p.since.Unix()
			v.Online++
			if ap := remoteAP(p.conn); ap.IsValid() {
				row.Endpoint = ap.String()
			}
		}
		p.mu.Unlock()
		row.Stats = p.stats.View()
		v.Players = append(v.Players, row)
	}
	return v
}

// cleanName режет имя, пришедшее от другого человека: длина и управляющие символы.
func cleanName(s string) string {
	out := make([]rune, 0, maxNameLen)
	for _, r := range strings.TrimSpace(s) {
		if r < 32 || r == 127 {
			continue
		}
		out = append(out, r)
		if len(out) >= maxNameLen {
			break
		}
	}
	return strings.TrimSpace(string(out))
}
