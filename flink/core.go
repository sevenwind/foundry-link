package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

type Options struct {
	Dir     string
	Local   bool // тестовый режим: только loopback, без STUN
	Version string
}

type Core struct {
	opts   Options
	st     *Store
	id     *Identity
	ctx    context.Context
	cancel context.CancelFunc
	logs   *LogBuf
	wake   chan struct{}

	mu     sync.Mutex
	net    *Net
	host   *Host
	player *Player
}

func NewCore(opts Options) (*Core, error) {
	st, err := openStore(opts.Dir)
	if err != nil {
		return nil, err
	}
	id, err := loadIdentity(opts.Dir)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &Core{opts: opts, st: st, id: id, ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1)}
	c.logs = newLogBuf(filepath.Join(opts.Dir, "flink.log"))
	return c, nil
}

func (c *Core) logf(format string, a ...any) { c.logs.add(fmt.Sprintf(format, a...)); c.changed() }

// changed будит подписчиков интерфейса.
func (c *Core) changed() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *Core) Close() {
	c.mu.Lock()
	if c.host != nil {
		c.host.stop()
		c.host = nil
	}
	if c.player != nil {
		c.player.stop()
		c.player = nil
	}
	if c.net != nil {
		c.net.Close()
		c.net = nil
	}
	c.mu.Unlock()
	c.cancel()
}

// Resume возобновляет режим, который был включён при прошлом запуске.
func (c *Core) Resume() {
	s := c.st.Get()
	switch {
	case s.Mode == "host" && s.HostEnabled:
		if err := c.StartHost(); err != nil {
			c.logf("Не удалось возобновить хост: %v", err)
		}
	case s.Mode == "player" && s.PlayerEnabled && s.RoomCode != "":
		if err := c.StartPlayer(s.RoomCode, s.PlayerName); err != nil {
			c.logf("Не удалось возобновить подключение: %v", err)
		}
	}
}

// ensureNetLocked создаёт UDP-сокеты и запускает обновление внешнего адреса. Вызывать под c.mu.
func (c *Core) ensureNetLocked() (*Net, error) {
	if c.net != nil {
		return c.net, nil
	}
	n, err := newNet(c.ctx, c.st.Get().UDPPort, c.opts.Local, c.logf)
	if err != nil {
		return nil, fmt.Errorf("не удалось открыть UDP-порт: %w", err)
	}
	c.net = n
	go func() {
		for {
			if n.Refresh(n.ctx) {
				c.changed()
			}
			wait := 30 * time.Second
			if v4, v6 := n.Endpoints(); !v4.IsValid() && !v6.IsValid() {
				wait = 4 * time.Second
			}
			select {
			case <-n.ctx.Done():
				return
			case <-time.After(wait):
			}
		}
	}()
	return n, nil
}

func (c *Core) releaseNetLocked() {
	if c.host == nil && c.player == nil && c.net != nil {
		c.net.Close()
		c.net = nil
	}
}

// ---- QUIC / TLS ----------------------------------------------------------

func quicConf() *quic.Config {
	return &quic.Config{
		HandshakeIdleTimeout:           7 * time.Second,
		MaxIdleTimeout:                 25 * time.Second,
		KeepAlivePeriod:                4 * time.Second,
		EnableDatagrams:                true,
		MaxIncomingStreams:             512,
		MaxIncomingUniStreams:          -1,
		InitialStreamReceiveWindow:     1 << 20,
		MaxStreamReceiveWindow:         8 << 20,
		InitialConnectionReceiveWindow: 2 << 20,
		MaxConnectionReceiveWindow:     24 << 20,
	}
}

// pinnedClientTLS: клиент проверяет, что сертификат хоста совпадает с отпечатком из кода комнаты.
func pinnedClientTLS(id *Identity, want [16]byte) *tls.Config {
	return &tls.Config{
		Certificates:       []tls.Certificate{id.Cert},
		MinVersion:         tls.VersionTLS13,
		NextProtos:         []string{alpn},
		ServerName:         tlsServerSNI,
		InsecureSkipVerify: true, // доверие даёт отпечаток, а не центр сертификации
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			fp, err := firstCertFP(raw)
			if err != nil {
				return err
			}
			if fp != want {
				return errors.New("отпечаток хоста не совпадает с кодом комнаты")
			}
			return nil
		},
	}
}

// ---- Управляющий поток ---------------------------------------------------

const (
	kindControl byte = 'C'
	kindTCP     byte = 'T'
)

type ctrlMsg struct {
	T      string `json:"t"`
	Name   string `json:"name,omitempty"`
	Host   string `json:"host,omitempty"`
	Max    int    `json:"max,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func writeMsg(w io.Writer, prefix []byte, m ctrlMsg) error {
	b, _ := json.Marshal(m)
	out := append([]byte{}, prefix...)
	out = binary.BigEndian.AppendUint16(out, uint16(len(b)))
	out = append(out, b...)
	_, err := w.Write(out)
	return err
}

func readMsg(r io.Reader) (ctrlMsg, error) {
	var m ctrlMsg
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return m, err
	}
	n := binary.BigEndian.Uint16(h[:])
	if n == 0 || n > 4096 {
		return m, errors.New("неверное сообщение")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
}

// ---- Журнал --------------------------------------------------------------

type LogBuf struct {
	mu    sync.Mutex
	lines []string
	file  *os.File
}

func newLogBuf(path string) *LogBuf {
	l := &LogBuf{}
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		if fi, err := f.Stat(); err == nil && fi.Size() > 1<<20 {
			_ = f.Truncate(0)
		}
		l.file = f
	}
	return l
}

func (l *LogBuf) add(s string) {
	line := time.Now().Format("15:04:05") + "  " + s
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, line)
	if len(l.lines) > 300 {
		l.lines = l.lines[len(l.lines)-300:]
	}
	if l.file != nil {
		_, _ = l.file.WriteString(time.Now().Format("2006-01-02 ") + line + "\n")
	}
}

func (l *LogBuf) tail(n int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.lines) < n {
		n = len(l.lines)
	}
	return append([]string(nil), l.lines[len(l.lines)-n:]...)
}

// ---- Состояние для интерфейса -------------------------------------------

type NatView struct {
	Ready bool   `json:"ready"`
	Level string `json:"level"`
	Text  string `json:"text"`
	V4    string `json:"v4"`
	V6    string `json:"v6"`
}

type SettingsView struct {
	HostName    string `json:"hostName"`
	PlayerName  string `json:"playerName"`
	FoundryPort int    `json:"foundryPort"`
	LocalPort   int    `json:"localPort"`
	RoomCode    string `json:"roomCode"`
}

type State struct {
	Version  string       `json:"version"`
	Mode     string       `json:"mode"`
	NAT      NatView      `json:"nat"`
	Host     *HostView    `json:"host"`
	Player   *PlayerView  `json:"player"`
	Settings SettingsView `json:"settings"`
	Log      []string     `json:"log"`
	MaxPlay  int          `json:"maxPlayers"`
	Update   *UpdateView  `json:"update"`
}

func (c *Core) State() State {
	c.mu.Lock()
	n, h, p := c.net, c.host, c.player
	c.mu.Unlock()
	s := c.st.Get()
	st := State{Version: c.opts.Version, Mode: s.Mode, MaxPlay: maxPlayers, Log: c.logs.tail(80), Update: latestUpdate.Load(),
		Settings: SettingsView{HostName: s.HostName, PlayerName: s.PlayerName, FoundryPort: s.FoundryPort, LocalPort: s.LocalPort, RoomCode: s.RoomCode}}
	if n != nil {
		v4, v6 := n.Endpoints()
		st.NAT.V4, st.NAT.V6 = addrStr(v4), addrStr(v6)
		st.NAT.Ready = v4.IsValid() || v6.IsValid()
		if st.NAT.Ready {
			st.NAT.Level, st.NAT.Text = natText(n.NAT(), v6.IsValid())
		}
	}
	if h != nil {
		st.Host = h.View()
	}
	if p != nil {
		st.Player = p.View()
	}
	return st
}

func addrStr(a netip.AddrPort) string {
	if !a.IsValid() {
		return ""
	}
	return a.String()
}

func shortID(id string) string {
	if len(id) > 4 {
		return strings.ToUpper(id[:4])
	}
	return id
}
