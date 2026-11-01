package main

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("таймаут ожидания: %s", what)
}

// mockFoundry — эхо-сервер, имитирующий Foundry на localhost.
func mockFoundry(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func newTestCore(t *testing.T) *Core {
	c, err := NewCore(Options{Dir: t.TempDir(), Local: true, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func hostCode(t *testing.T, h *Core) string {
	waitFor(t, 5*time.Second, "код комнаты", func() bool { s := h.State(); return s.Host != nil && s.Host.Code != "" })
	return h.State().Host.Code
}

func replyCode(t *testing.T, p *Core) string {
	waitFor(t, 5*time.Second, "ответный код", func() bool { s := p.State(); return s.Player != nil && s.Player.ReplyCode != "" })
	return p.State().Player.ReplyCode
}

// echoCheck отправляет n байт через локальный порт игрока и проверяет, что вернулось то же самое.
func echoCheck(port, n int) error {
	c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	defer c.Close()
	data := make([]byte, n)
	rand.Read(data)
	want := sha256.Sum256(data)
	errc := make(chan error, 1)
	go func() {
		_, err := c.Write(data)
		if err == nil {
			err = c.(*net.TCPConn).CloseWrite()
		}
		errc <- err
	}()
	h := sha256.New()
	got, err := io.Copy(h, c)
	if err != nil {
		return err
	}
	if err := <-errc; err != nil {
		return err
	}
	if int(got) != n {
		return fmt.Errorf("получено %d байт вместо %d", got, n)
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	if sum != want {
		return fmt.Errorf("данные повреждены")
	}
	return nil
}

func TestElevenPlayers(t *testing.T) {
	fport := mockFoundry(t)
	host := newTestCore(t)
	host.st.Update(func(s *Settings) { s.FoundryPort = fport })
	if err := host.StartHost(); err != nil {
		t.Fatal(err)
	}
	code := hostCode(t, host)

	var players []*Core
	for i := 0; i < maxPlayers; i++ {
		p := newTestCore(t)
		if err := p.StartPlayer(code, fmt.Sprintf("Игрок%d", i+1)); err != nil {
			t.Fatal(err)
		}
		players = append(players, p)
		n, errs := host.AddPlayers(replyCode(t, p))
		if n != 1 || len(errs) != 0 {
			t.Fatalf("игрок %d не добавлен: %v", i+1, errs)
		}
	}

	waitFor(t, 20*time.Second, "все 11 онлайн", func() bool { return host.State().Host.Online == maxPlayers })

	// 12-й игрок отклоняется
	extra := newTestCore(t)
	if err := extra.StartPlayer(code, "Лишний"); err != nil {
		t.Fatal(err)
	}
	n, errs := host.AddPlayers(replyCode(t, extra))
	if n != 0 || len(errs) == 0 || !strings.Contains(errs[0], "предел") {
		t.Fatalf("12-й игрок должен быть отклонён, n=%d errs=%v", n, errs)
	}

	// Передача данных: все игроки одновременно, по 3 соединения, 4 МБ каждое
	var wg sync.WaitGroup
	errc := make(chan error, 100)
	start := time.Now()
	for i, p := range players {
		port := p.State().Player.LocalPort
		for k := 0; k < 3; k++ {
			wg.Add(1)
			go func(i, port int) {
				defer wg.Done()
				if err := echoCheck(port, 4<<20); err != nil {
					errc <- fmt.Errorf("игрок %d: %w", i+1, err)
				}
			}(i, port)
		}
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Error(err)
	}
	t.Logf("33 соединения × 4 МБ туда-обратно (%d МБ) за %.1f с", 33*8, time.Since(start).Seconds())

	// Пинг и имена
	waitFor(t, 8*time.Second, "пинг у всех", func() bool {
		for _, r := range host.State().Host.Players {
			if !r.Stats.Have {
				return false
			}
		}
		return true
	})
	for _, r := range host.State().Host.Players {
		if r.Name == "" || r.State != "online" {
			t.Errorf("плохая строка игрока: %+v", r)
		}
		if r.Stats.Ping > 200 {
			t.Errorf("слишком большой пинг на loopback: %+v", r.Stats)
		}
	}
	for _, p := range players {
		v := p.State().Player
		if v.State != "online" || !v.Stats.Have {
			t.Errorf("игрок не в порядке: %+v", v)
		}
	}
}

func TestReconnectAndRemove(t *testing.T) {
	fport := mockFoundry(t)
	host := newTestCore(t)
	host.st.Update(func(s *Settings) { s.FoundryPort = fport })
	if err := host.StartHost(); err != nil {
		t.Fatal(err)
	}
	code := hostCode(t, host)
	p := newTestCore(t)
	if err := p.StartPlayer(code, "Вася"); err != nil {
		t.Fatal(err)
	}

	// Пока мастер не добавил игрока, тот видит понятное состояние
	waitFor(t, 15*time.Second, "состояние notadded", func() bool { return p.State().Player.State == "notadded" })

	host.AddPlayers(replyCode(t, p))
	waitFor(t, 15*time.Second, "онлайн", func() bool { return p.State().Player.State == "online" })
	port := p.State().Player.LocalPort
	if err := echoCheck(port, 1<<20); err != nil {
		t.Fatal(err)
	}

	// Обрыв соединения: игрок должен переподключиться сам
	p.player.mu.Lock()
	p.player.conn.CloseWithError(99, "тест обрыва")
	p.player.mu.Unlock()
	waitFor(t, 20*time.Second, "переподключение", func() bool {
		v := p.State().Player
		return v.State == "online" && v.Reconnects == 1
	})
	waitFor(t, 5*time.Second, "хост видит игрока онлайн", func() bool { return host.State().Host.Online == 1 })
	if err := echoCheck(port, 1<<20); err != nil {
		t.Fatalf("после переподключения: %v", err)
	}

	// Перезапуск хоста: тот же код комнаты не нужен, но игрок возвращается сам
	host.StopHost()
	host.st.Update(func(s *Settings) { s.FoundryPort = fport })
	if err := host.StartHost(); err != nil {
		t.Fatal(err)
	}
	newCode := hostCode(t, host)
	t.Logf("код хоста после перезапуска изменился: %v (loopback-порт новый)", newCode != code)

	// Удаление игрока
	id := host.State().Host.Players[0].ID
	host.RemovePlayer(id)
	waitFor(t, 20*time.Second, "игрок видит удаление", func() bool { return p.State().Player.State != "online" })
	if n := len(host.State().Host.Players); n != 0 {
		t.Fatalf("игрок не удалён: %d", n)
	}
}

func TestForcedV4(t *testing.T) {
	fport := mockFoundry(t)
	host := newTestCore(t)
	host.st.Update(func(s *Settings) { s.FoundryPort = fport })
	if err := host.StartHost(); err != nil {
		t.Fatal(err)
	}
	c, err := DecodeCode(hostCode(t, host))
	if err != nil {
		t.Fatal(err)
	}
	c.V6 = netipZero()
	p := newTestCore(t)
	if err := p.StartPlayer(c.Encode(), "V4"); err != nil {
		t.Fatal(err)
	}
	host.AddPlayers(replyCode(t, p))
	waitFor(t, 15*time.Second, "онлайн по IPv4", func() bool { return p.State().Player.State == "online" })
	if got := p.State().Player.Path; got != "IPv4" {
		t.Fatalf("ожидался IPv4, путь %q", got)
	}
	if err := echoCheck(p.State().Player.LocalPort, 2<<20); err != nil {
		t.Fatal(err)
	}
}
