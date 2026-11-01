package main

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"time"
)

//go:embed web
var webFS embed.FS

type UI struct {
	c     *Core
	port  int
	token string
}

func newUI(c *Core, port int) *UI {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return &UI{c: c, port: port, token: hex.EncodeToString(b[:])}
}

// SetMode переключает роль и останавливает то, что работало раньше.
func (c *Core) SetMode(mode string) error {
	switch mode {
	case "host":
		return c.StartHost()
	case "player", "":
		c.StopHost()
		c.StopPlayer()
		c.st.Update(func(s *Settings) { s.Mode = mode })
		return nil
	}
	return fmt.Errorf("неизвестная роль %q", mode)
}

func (u *UI) hostOK(r *http.Request) bool {
	return r.Host == fmt.Sprintf("127.0.0.1:%d", u.port) || r.Host == fmt.Sprintf("localhost:%d", u.port)
}

func (u *UI) tokenOK(r *http.Request) bool {
	t := r.Header.Get("X-FL-Token")
	if t == "" {
		t = r.URL.Query().Get("t")
	}
	return t == u.token
}

func (u *UI) handler() http.Handler {
	sub, _ := fs.Sub(webFS, "web")
	static := http.FileServer(http.FS(sub))
	mux := http.NewServeMux()

	mux.HandleFunc("/api/ping", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "foundrylink") })

	mux.HandleFunc("/api/state", u.guard(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, u.c.State())
	}))

	mux.HandleFunc("/api/events", u.guard(func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		send := func() {
			b, _ := json.Marshal(u.c.State())
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}
		send()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-t.C:
				send()
			}
		}
	}))

	post := func(path string, f func(r *http.Request) (any, error)) {
		mux.HandleFunc(path, u.guard(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "POST only", 405)
				return
			}
			extra, err := f(r)
			if err != nil {
				writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
				return
			}
			writeJSON(w, map[string]any{"ok": true, "state": u.c.State(), "extra": extra})
		}))
	}
	body := func(r *http.Request, v any) error {
		r.Body = http.MaxBytesReader(nil, r.Body, 1<<20)
		return json.NewDecoder(r.Body).Decode(v)
	}

	post("/api/mode", func(r *http.Request) (any, error) {
		var in struct{ Mode string }
		if err := body(r, &in); err != nil {
			return nil, err
		}
		return nil, u.c.SetMode(in.Mode)
	})
	post("/api/host/start", func(r *http.Request) (any, error) { return nil, u.c.StartHost() })
	post("/api/host/stop", func(r *http.Request) (any, error) { u.c.StopHost(); return nil, nil })
	post("/api/host/add", func(r *http.Request) (any, error) {
		var in struct{ Codes string }
		if err := body(r, &in); err != nil {
			return nil, err
		}
		n, errs := u.c.AddPlayers(in.Codes)
		if n == 0 && len(errs) > 0 {
			return nil, fmt.Errorf("%s", strings.Join(errs, "; "))
		}
		return map[string]any{"added": n, "errors": errs}, nil
	})
	post("/api/host/remove", func(r *http.Request) (any, error) {
		var in struct{ ID string }
		if err := body(r, &in); err != nil {
			return nil, err
		}
		u.c.RemovePlayer(in.ID)
		return nil, nil
	})
	post("/api/host/settings", func(r *http.Request) (any, error) {
		var in struct {
			Name        string
			FoundryPort int
		}
		if err := body(r, &in); err != nil {
			return nil, err
		}
		return nil, u.c.SetHostSettings(in.Name, in.FoundryPort)
	})
	post("/api/player/start", func(r *http.Request) (any, error) {
		var in struct{ Code, Name string }
		if err := body(r, &in); err != nil {
			return nil, err
		}
		return nil, u.c.StartPlayer(in.Code, in.Name)
	})
	post("/api/player/stop", func(r *http.Request) (any, error) { u.c.StopPlayer(); return nil, nil })
	post("/api/player/settings", func(r *http.Request) (any, error) {
		var in struct{ LocalPort int }
		if err := body(r, &in); err != nil {
			return nil, err
		}
		return nil, u.c.SetLocalPort(in.LocalPort)
	})
	post("/api/quit", func(r *http.Request) (any, error) {
		go func() {
			time.Sleep(200 * time.Millisecond)
			u.c.Close()
			os.Exit(0)
		}()
		return nil, nil
	})

	// Главная страница получает токен; остальное — статика.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !u.hostOK(r) {
			http.Error(w, "forbidden", 403)
			return
		}
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			static.ServeHTTP(w, r)
			return
		}
		b, err := fs.ReadFile(sub, "index.html")
		if err != nil {
			http.Error(w, "no ui", 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(strings.ReplaceAll(string(b), "{{TOKEN}}", u.token)))
	})
	return mux
}

// guard защищает API от чужих сайтов: проверка Host (DNS-rebinding) и токена (CSRF).
func (u *UI) guard(f http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !u.hostOK(r) || !u.tokenOK(r) {
			http.Error(w, "forbidden", 403)
			return
		}
		f(w, r)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}
