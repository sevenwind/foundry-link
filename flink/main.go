// Foundry Link — P2P-соединение для игры в Foundry VTT без VPN и серверов.
//
//	flink                запуск (откроет интерфейс в браузере)
//	flink -no-browser    без открытия браузера
//	flink -data DIR      папка с настройками (по умолчанию в профиле пользователя)
//	flink -ui-port N     порт локального интерфейса (по умолчанию 17630)
package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

const version = "1.1.1"

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

// alreadyRunning проверяет, что на порту уже отвечает наше приложение.
func alreadyRunning(port int) bool {
	c := http.Client{Timeout: 700 * time.Millisecond}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/api/ping", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 32))
	return string(b) == "foundrylink"
}

func main() {
	dataDir := flag.String("data", "", "папка с настройками")
	uiPort := flag.Int("ui-port", 17630, "порт локального интерфейса")
	noBrowser := flag.Bool("no-browser", false, "не открывать браузер")
	local := flag.Bool("local", false, "тестовый режим: только loopback, без STUN")
	showVersion := flag.Bool("version", false, "показать версию")
	flag.Parse()
	if *showVersion {
		fmt.Println("Foundry Link", version)
		return
	}

	dir := *dataDir
	if dir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			base = os.TempDir()
		}
		dir = filepath.Join(base, "FoundryLink")
	}

	// Один экземпляр: если уже запущено — просто открываем интерфейс.
	var ln net.Listener
	port := *uiPort
	for i := 0; i < 20; i++ {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port+i))
		if err == nil {
			ln, port = l, port+i
			break
		}
		if alreadyRunning(port + i) {
			url := fmt.Sprintf("http://127.0.0.1:%d/", port+i)
			fmt.Println("FLINK_UI", url, "(уже запущено)")
			if !*noBrowser {
				openBrowser(url)
			}
			return
		}
	}
	if ln == nil {
		fmt.Fprintln(os.Stderr, "не удалось открыть порт интерфейса")
		os.Exit(1)
	}

	core, err := NewCore(Options{Dir: dir, Local: *local, Version: version})
	if err != nil {
		fmt.Fprintln(os.Stderr, "ошибка запуска:", err)
		os.Exit(1)
	}
	ui := newUI(core, port)
	core.Resume()
	core.startUpdateCheck()

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		core.Close()
		os.Exit(0)
	}()

	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	fmt.Println("FLINK_UI", url)
	fmt.Println("Foundry Link работает. Закройте это окно или нажмите «Выйти» в интерфейсе, чтобы завершить.")
	if !*noBrowser {
		openBrowser(url)
	}
	srv := &http.Server{Handler: ui.handler()}
	if err := srv.Serve(ln); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}
