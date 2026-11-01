package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
)

type Player struct {
	c      *Core
	net    *Net
	room   Code
	name   string
	ctx    context.Context
	cancel context.CancelFunc

	mu         sync.Mutex
	state      string // connecting | online | reconnecting | notadded | error
	msg        string
	conn       quic.Connection
	hostName   string
	path       string
	since      time.Time
	reconnects int
	everOnline bool
	localPort  int
	listeners  []net.Listener

	stats      *Stats
	sent, recv atomic.Int64
}

func (c *Core) StartPlayer(roomCode, name string) error {
	code, err := DecodeCode(roomCode)
	if err != nil {
		return err
	}
	if code.Role != roleHost {
		return errors.New("это ответный код игрока, а нужен код комнаты от мастера")
	}
	if code.FP == c.id.FP {
		return errors.New("это ваш собственный код")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Игрок"
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.player != nil {
		c.player.stop()
		c.player = nil
	}
	if c.host != nil {
		c.host.stop()
		c.host = nil
	}
	n, err := c.ensureNetLocked()
	if err != nil {
		return err
	}
	p := &Player{c: c, net: n, room: code, name: name, state: "connecting", stats: newStats()}
	p.ctx, p.cancel = context.WithCancel(c.ctx)

	ln, port, err := listenLocal(c.st.Get().LocalPort)
	if err != nil {
		p.cancel()
		c.releaseNetLocked()
		return fmt.Errorf("не удалось открыть локальный порт: %w", err)
	}
	p.localPort = port
	p.listeners = append(p.listeners, ln)
	if ln6, err := net.Listen("tcp", fmt.Sprintf("[::1]:%d", port)); err == nil {
		p.listeners = append(p.listeners, ln6)
	}
	for _, l := range p.listeners {
		go p.acceptLocal(l)
	}
	go p.supervise()

	c.player = p
	c.st.Update(func(s *Settings) {
		s.Mode, s.PlayerEnabled, s.RoomCode, s.PlayerName = "player", true, strings.ToUpper(roomCode), name
	})
	c.logf("Подключаюсь к мастеру, локальный адрес: localhost:%d", port)
	return nil
}

func (c *Core) StopPlayer() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.player != nil {
		c.player.stop()
		c.player = nil
		c.releaseNetLocked()
		c.logf("Отключено")
	}
	c.st.Update(func(s *Settings) { s.PlayerEnabled = false })
}

func (c *Core) SetLocalPort(port int) error {
	if port < 1 || port > 65535 {
		return errors.New("порт должен быть от 1 до 65535")
	}
	c.st.Update(func(s *Settings) { s.LocalPort = port })
	c.changed()
	return nil
}

func (p *Player) stop() {
	p.cancel()
	for _, l := range p.listeners {
		_ = l.Close()
	}
	p.mu.Lock()
	if p.conn != nil {
		p.conn.CloseWithError(10, "player stopped")
	}
	p.mu.Unlock()
}

func listenLocal(pref int) (net.Listener, int, error) {
	for i := 0; i < 50 && pref+i <= 65535; i++ {
		if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", pref+i)); err == nil {
			return ln, pref + i, nil
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, 0, err
	}
	return ln, ln.Addr().(*net.TCPAddr).Port, nil
}

func (p *Player) setState(state, msg string) {
	p.mu.Lock()
	changed := p.state != state || p.msg != msg
	p.state, p.msg = state, msg
	p.mu.Unlock()
	if changed {
		p.c.changed()
	}
}

// ---- Подключение к мастеру ----------------------------------------------

var errNotAdded = errors.New("мастер ещё не добавил ваш ответный код")

func (p *Player) supervise() {
	backoff := 500 * time.Millisecond
	first := true
	fails := 0
	for p.ctx.Err() == nil {
		msg := "Пробиваю соединение с мастером…"
		state := "connecting"
		if !first {
			msg, state = "Связь потеряна, переподключаюсь…", "reconnecting"
		}
		if fails >= 3 {
			msg = "Нет ответа от мастера. Он запущен? Код комнаты мог измениться: попросите новый"
		}
		p.setState(state, msg)
		conn, fam, err := p.dial()
		var welcome ctrlMsg
		var cs quic.Stream
		if err == nil {
			cs, welcome, err = p.handshake(conn)
			if err != nil {
				conn.CloseWithError(6, "handshake failed")
			}
		}
		if err != nil {
			if p.ctx.Err() != nil {
				return
			}
			if isCryptoErr(err) || errors.Is(err, errNotAdded) {
				p.setState("notadded", "Мастер ещё не добавил вас: отправьте ему свой ответный код")
				sleepCtx(p.ctx, 3*time.Second)
				continue
			}
			fails++
			if fails <= 3 || fails%10 == 0 {
				p.c.logf("Не удалось подключиться: %v", err)
			}
			sleepCtx(p.ctx, backoff)
			if backoff < 3*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = 500 * time.Millisecond
		first = false
		fails = 0

		p.mu.Lock()
		p.conn = conn
		p.hostName = welcome.Host
		p.stats.reset()
		p.since = time.Now()
		if p.everOnline {
			p.reconnects++
		}
		p.everOnline = true
		p.path = map[string]string{"v4": "IPv4", "v6": "IPv6"}[fam]
		p.mu.Unlock()
		p.setState("online", "")
		p.c.logf("Соединение установлено (%s)", p.path)

		go runPinger(conn.Context(), conn, p.stats, func() { conn.CloseWithError(5, "watchdog") })
		go func() {
			for {
				if _, err := readMsg(cs); err != nil {
					conn.CloseWithError(0, "control closed")
					return
				}
			}
		}()
		<-conn.Context().Done()
		p.mu.Lock()
		if p.conn == conn {
			p.conn = nil
		}
		p.mu.Unlock()
		p.c.logf("Связь с мастером потеряна")
	}
}

func isCryptoErr(err error) bool {
	var te *quic.TransportError
	return errors.As(err, &te) && te.ErrorCode.IsCryptoError()
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

type dialRes struct {
	conn quic.Connection
	fam  string
	err  error
}

// dial пробует IPv6 и IPv4 одновременно (IPv6 стартует первым) и берёт то, что соединилось первым.
func (p *Player) dial() (quic.Connection, string, error) {
	type cand struct {
		f     *family
		ep    netip.AddrPort
		delay time.Duration
	}
	var cands []cand
	if p.room.V6.IsValid() {
		if f := p.net.family("v6"); f != nil {
			cands = append(cands, cand{f, p.room.V6, 0})
		}
	}
	if p.room.V4.IsValid() {
		if f := p.net.family("v4"); f != nil {
			d := time.Duration(0)
			if len(cands) > 0 {
				d = 250 * time.Millisecond
			}
			cands = append(cands, cand{f, p.room.V4, d})
		}
	}
	if len(cands) == 0 {
		return nil, "", errors.New("нет общего семейства адресов (IPv4/IPv6) с мастером")
	}
	results := make(chan dialRes, len(cands))
	cancels := make([]context.CancelFunc, len(cands))
	for i, c := range cands {
		ctx, cancel := context.WithTimeout(p.ctx, 8*time.Second)
		cancels[i] = cancel
		go func(c cand, ctx context.Context) {
			select {
			case <-time.After(c.delay):
			case <-ctx.Done():
				results <- dialRes{err: ctx.Err()}
				return
			}
			stop := make(chan struct{})
			go func() { // «стук» в наш NAT в сторону мастера
				t := time.NewTicker(400 * time.Millisecond)
				defer t.Stop()
				for {
					p.net.SendHello(c.ep)
					select {
					case <-stop:
						return
					case <-ctx.Done():
						return
					case <-t.C:
					}
				}
			}()
			conn, err := c.f.tr.Dial(ctx, net.UDPAddrFromAddrPort(c.ep), pinnedClientTLS(p.c.id, p.room.FP), quicConf())
			close(stop)
			results <- dialRes{conn, c.f.name, err}
		}(c, ctx)
	}
	var win *dialRes
	var lastErr error
	for range cands {
		r := <-results
		switch {
		case r.err != nil:
			if lastErr == nil || !errors.Is(r.err, context.Canceled) {
				lastErr = r.err
			}
		case win == nil:
			rr := r
			win = &rr
			for _, cancel := range cancels { // остальные попытки больше не нужны
				cancel()
			}
		default:
			r.conn.CloseWithError(7, "lost the race")
		}
	}
	if win != nil {
		return win.conn, win.fam, nil
	}
	if lastErr == nil {
		lastErr = errors.New("нет ответа от мастера")
	}
	return nil, "", lastErr
}

func (p *Player) handshake(conn quic.Connection) (quic.Stream, ctrlMsg, error) {
	ctx, cancel := context.WithTimeout(conn.Context(), 6*time.Second)
	defer cancel()
	cs, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, ctrlMsg{}, err
	}
	if err := writeMsg(cs, []byte{kindControl}, ctrlMsg{T: "hello", Name: p.name}); err != nil {
		return nil, ctrlMsg{}, err
	}
	_ = cs.SetReadDeadline(time.Now().Add(6 * time.Second))
	m, err := readMsg(cs)
	if err != nil {
		if isCryptoErr(err) {
			return nil, m, err
		}
		var ae *quic.ApplicationError
		if errors.As(err, &ae) && (ae.ErrorCode == 2 || ae.ErrorCode == 11) {
			return nil, m, errNotAdded
		}
		return nil, m, err
	}
	_ = cs.SetReadDeadline(time.Time{})
	if m.T == "deny" {
		return nil, m, errNotAdded
	}
	if m.T != "welcome" {
		return nil, m, errors.New("неожиданный ответ мастера")
	}
	return cs, m, nil
}

// ---- Локальный порт для браузера ----------------------------------------

func (p *Player) waitConn(d time.Duration) quic.Connection {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) && p.ctx.Err() == nil {
		p.mu.Lock()
		c := p.conn
		p.mu.Unlock()
		if c != nil {
			return c
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil
}

func (p *Player) acceptLocal(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go p.handleLocal(c.(*net.TCPConn))
	}
}

func (p *Player) handleLocal(tcp *net.TCPConn) {
	conn := p.waitConn(6 * time.Second)
	if conn == nil {
		_ = tcp.Close()
		return
	}
	ctx, cancel := context.WithTimeout(conn.Context(), 5*time.Second)
	st, err := conn.OpenStreamSync(ctx)
	cancel()
	if err != nil {
		_ = tcp.Close()
		return
	}
	if _, err := st.Write([]byte{kindTCP}); err != nil {
		_ = tcp.Close()
		return
	}
	pipe(tcp, st, &p.sent, &p.recv)
}

// ---- Представление для интерфейса ---------------------------------------

type PlayerView struct {
	Running    bool      `json:"running"`
	State      string    `json:"state"`
	Message    string    `json:"message"`
	ReplyCode  string    `json:"replyCode"`
	LocalPort  int       `json:"localPort"`
	LocalURL   string    `json:"localUrl"`
	HostName   string    `json:"hostName"`
	Path       string    `json:"path"`
	Stats      StatsView `json:"stats"`
	Sent       int64     `json:"sent"`
	Recv       int64     `json:"recv"`
	Since      int64     `json:"since"`
	Reconnects int       `json:"reconnects"`
	HostNAT    string    `json:"hostNat"`
}

func (p *Player) View() *PlayerView {
	v4, v6 := p.net.Endpoints()
	p.mu.Lock()
	v := &PlayerView{Running: true, State: p.state, Message: p.msg, LocalPort: p.localPort,
		LocalURL: fmt.Sprintf("http://localhost:%d", p.localPort), HostName: p.hostName, Path: p.path,
		Sent: p.sent.Load(), Recv: p.recv.Load(), Reconnects: p.reconnects}
	if p.conn != nil {
		v.Since = p.since.Unix()
	}
	p.mu.Unlock()
	if v4.IsValid() || v6.IsValid() {
		v.ReplyCode = Code{Role: rolePlayer, NAT: p.net.NAT(), FP: p.c.id.FP, V4: v4, V6: v6, Name: p.name}.Encode()
	}
	v.Stats = p.stats.View()
	_, v.HostNAT = natText(p.room.NAT, p.room.V6.IsValid())
	return v
}
