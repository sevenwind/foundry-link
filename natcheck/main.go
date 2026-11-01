// natcheck: проверяет, дойдёт ли до вас P2P-соединение.
//
//	natcheck          — меню (удобно при запуске двойным щелчком)
//	natcheck stun     — определить внешний адрес и тип NAT через STUN
//	natcheck host     — парный тест: создать код и ждать второго участника
//	natcheck join     — парный тест: ввести код хоста
//
// Работает на macOS и Windows, без зависимостей.
package main

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"
)

var report strings.Builder

// lines — единственный читатель stdin: так ввод кода, отмена по Enter и финальное
// «нажмите Enter» не мешают друг другу.
var lines = make(chan string)

func startInput() {
	go func() {
		r := bufio.NewReader(os.Stdin)
		for {
			s, err := r.ReadString('\n')
			if s != "" || err == nil {
				lines <- strings.TrimSpace(s)
			}
			if err != nil {
				close(lines)
				return
			}
		}
	}()
}

func readLine() string {
	s := <-lines
	return s
}

func out(format string, a ...any) {
	s := fmt.Sprintf(format, a...)
	fmt.Println(s)
	report.WriteString(s + "\n")
}

func header(title string) {
	report.Reset()
	out("%s", title)
	out("Система: %s/%s, время: %s", runtime.GOOS, runtime.GOARCH, time.Now().Format("2006-01-02 15:04:05"))
	out("")
}

func writeReport(name string) {
	if err := os.WriteFile(name, []byte(report.String()), 0o644); err == nil {
		fmt.Printf("\nОтчёт сохранён: %s (отправьте его мне)\n", name)
	}
}

func waitEnter() {
	fmt.Print("\nНажмите Enter, чтобы закрыть…")
	<-lines
}

func menu() string {
	fmt.Println("natcheck — проверка связности для Foundry Link")
	fmt.Println()
	fmt.Println("  1 — я хост (создаю код для второго участника)")
	fmt.Println("  2 — я второй участник (у меня есть код хоста)")
	fmt.Println("  3 — только проверка STUN (без второго участника)")
	fmt.Println()
	for {
		fmt.Print("Выберите 1, 2 или 3: ")
		s, ok := <-lines
		if !ok {
			return "stun"
		}
		switch s {
		case "1":
			return "host"
		case "2":
			return "join"
		case "3":
			return "stun"
		}
	}
}

func main() {
	startInput()

	mode := ""
	var extra []string
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "host", "join", "stun":
			mode, extra = os.Args[1], os.Args[2:]
		default: // старый формат: аргументы — дополнительные STUN-серверы
			mode, extra = "stun", os.Args[1:]
		}
	} else {
		mode = menu()
		fmt.Println()
	}

	switch mode {
	case "host":
		pairMode(true, extra)
	case "join":
		pairMode(false, extra)
	default:
		stunMode(extra)
	}
	waitEnter()
}
