// Оболочка Foundry Link для macOS: нативное окно с WKWebView и вшитый сетевой движок `flink`.
// Закрытие окна не останавливает игру: движок работает, пока приложение не завершено (⌘Q).
import Cocoa
import WebKit

final class AppDelegate: NSObject, NSApplicationDelegate, WKNavigationDelegate, WKUIDelegate {
    var window: NSWindow!
    var web: WKWebView!
    var core: Process?
    var uiURL: URL?

    func applicationDidFinishLaunching(_ n: Notification) {
        buildMenu()
        web = WKWebView(frame: .zero, configuration: WKWebViewConfiguration())
        web.navigationDelegate = self
        web.uiDelegate = self
        web.setValue(false, forKey: "drawsBackground")

        window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 1020, height: 820),
                          styleMask: [.titled, .closable, .miniaturizable, .resizable],
                          backing: .buffered, defer: false)
        window.title = "Foundry Link"
        window.contentView = web
        window.minSize = NSSize(width: 560, height: 480)
        window.setFrameAutosaveName("FoundryLinkMain")
        if !window.setFrameUsingName("FoundryLinkMain") { window.center() }
        window.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
        web.loadHTMLString("<body style='background:#12141a;color:#98a0b3;font:15px system-ui;display:grid;place-items:center;height:100vh;margin:0'>Запуск…</body>", baseURL: nil)
        startCore()
    }

    func startCore() {
        guard let exe = Bundle.main.url(forResource: "flink", withExtension: nil) else {
            fail("В приложении не найден сетевой движок (flink).")
            return
        }
        let p = Process()
        p.executableURL = exe
        p.arguments = ["-no-browser"]
        let pipe = Pipe()
        p.standardOutput = pipe
        p.standardError = pipe
        var buf = Data()
        pipe.fileHandleForReading.readabilityHandler = { [weak self] h in
            buf.append(h.availableData)
            while let nl = buf.firstIndex(of: 0x0A) {
                let line = String(decoding: buf[buf.startIndex..<nl], as: UTF8.self)
                buf.removeSubrange(buf.startIndex...nl)
                if line.hasPrefix("FLINK_UI "), let s = line.dropFirst(9).split(separator: " ").first, let url = URL(string: String(s)) {
                    DispatchQueue.main.async { self?.uiURL = url; self?.web.load(URLRequest(url: url)) }
                }
            }
        }
        p.terminationHandler = { [weak self] _ in
            DispatchQueue.main.async { if self?.uiURL == nil { self?.fail("Сетевой движок не запустился.") } }
        }
        do { try p.run(); core = p } catch { fail("Не удалось запустить движок: \(error.localizedDescription)") }
    }

    func fail(_ msg: String) {
        web.loadHTMLString("<body style='background:#12141a;color:#ff6b6b;font:15px system-ui;display:grid;place-items:center;height:100vh;margin:0'>\(msg)</body>", baseURL: nil)
    }

    // MARK: окно и завершение
    func applicationShouldTerminateAfterLastWindowClosed(_ s: NSApplication) -> Bool { false }

    func applicationShouldHandleReopen(_ s: NSApplication, hasVisibleWindows f: Bool) -> Bool {
        window.makeKeyAndOrderFront(nil)
        return true
    }

    func applicationShouldTerminate(_ s: NSApplication) -> NSApplication.TerminateReply {
        let a = NSAlert()
        a.messageText = "Выйти из Foundry Link?"
        a.informativeText = "Все подключения (мастера и игроков) будут разорваны."
        a.addButton(withTitle: "Выйти")
        a.addButton(withTitle: "Отмена")
        return a.runModal() == .alertFirstButtonReturn ? .terminateNow : .terminateCancel
    }

    func applicationWillTerminate(_ n: Notification) { core?.terminate() }

    // MARK: навигация: внешние ссылки (Foundry) открываем в браузере
    func webView(_ w: WKWebView, decidePolicyFor a: WKNavigationAction, decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
        if let url = a.request.url, url.scheme == "http" || url.scheme == "https",
           url.host != uiURL?.host || url.port != uiURL?.port {
            NSWorkspace.shared.open(url)
            decisionHandler(.cancel)
            return
        }
        if a.targetFrame == nil, let url = a.request.url { // target=_blank
            NSWorkspace.shared.open(url)
            decisionHandler(.cancel)
            return
        }
        decisionHandler(.allow)
    }

    func webView(_ w: WKWebView, createWebViewWith c: WKWebViewConfiguration, for a: WKNavigationAction, windowFeatures f: WKWindowFeatures) -> WKWebView? {
        if let url = a.request.url { NSWorkspace.shared.open(url) }
        return nil
    }

    // JS confirm()/alert() иначе молча возвращают false
    func webView(_ w: WKWebView, runJavaScriptConfirmPanelWithMessage m: String, initiatedByFrame f: WKFrameInfo, completionHandler h: @escaping (Bool) -> Void) {
        let a = NSAlert(); a.messageText = m
        a.addButton(withTitle: "OK"); a.addButton(withTitle: "Отмена")
        h(a.runModal() == .alertFirstButtonReturn)
    }

    func webView(_ w: WKWebView, runJavaScriptAlertPanelWithMessage m: String, initiatedByFrame f: WKFrameInfo, completionHandler h: @escaping () -> Void) {
        let a = NSAlert(); a.messageText = m; a.runModal(); h()
    }

    // MARK: меню (нужно для ⌘C/⌘V/⌘A и ⌘Q)
    func buildMenu() {
        let main = NSMenu()
        let appItem = NSMenuItem(); main.addItem(appItem)
        let app = NSMenu(); appItem.submenu = app
        app.addItem(withTitle: "О Foundry Link", action: #selector(NSApplication.orderFrontStandardAboutPanel(_:)), keyEquivalent: "")
        app.addItem(.separator())
        app.addItem(withTitle: "Скрыть Foundry Link", action: #selector(NSApplication.hide(_:)), keyEquivalent: "h")
        app.addItem(.separator())
        app.addItem(withTitle: "Выйти из Foundry Link", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")

        let editItem = NSMenuItem(); main.addItem(editItem)
        let edit = NSMenu(title: "Правка"); editItem.submenu = edit
        edit.addItem(withTitle: "Отменить", action: Selector(("undo:")), keyEquivalent: "z")
        edit.addItem(.separator())
        edit.addItem(withTitle: "Вырезать", action: #selector(NSText.cut(_:)), keyEquivalent: "x")
        edit.addItem(withTitle: "Копировать", action: #selector(NSText.copy(_:)), keyEquivalent: "c")
        edit.addItem(withTitle: "Вставить", action: #selector(NSText.paste(_:)), keyEquivalent: "v")
        edit.addItem(withTitle: "Выбрать всё", action: #selector(NSText.selectAll(_:)), keyEquivalent: "a")

        let winItem = NSMenuItem(); main.addItem(winItem)
        let win = NSMenu(title: "Окно"); winItem.submenu = win
        win.addItem(withTitle: "Свернуть", action: #selector(NSWindow.performMiniaturize(_:)), keyEquivalent: "m")
        win.addItem(withTitle: "Закрыть окно", action: #selector(NSWindow.performClose(_:)), keyEquivalent: "w")
        NSApp.mainMenu = main
    }
}

let app = NSApplication.shared
let delegate = AppDelegate()
app.delegate = delegate
app.setActivationPolicy(.regular)
app.run()
