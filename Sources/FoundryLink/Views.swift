import SwiftUI
import AppKit

struct RootView: View {
    @EnvironmentObject var store: Store

    var body: some View {
        VStack(spacing: 0) {
            header
            if let e = store.error {
                Text(e).font(.callout).foregroundStyle(.white)
                    .frame(maxWidth: .infinity, alignment: .leading).padding(8).background(.red.opacity(0.85))
            }
            TabView {
                JoinView().tabItem { Label("Подключение", systemImage: "network") }
                HostView().tabItem { Label("Хостинг", systemImage: "server.rack") }
            }
        }
        .frame(minWidth: 640, minHeight: 520)
        .onAppear { store.start() }
    }

    var header: some View {
        HStack {
            Circle().fill(store.status?.online == true ? .green : .orange).frame(width: 10, height: 10)
            Text(store.status.map { $0.online ? "В сети" : "Нет связи с корневыми серверами" } ?? "Служба недоступна")
            Spacer()
            if let s = store.status {
                Text("Мой ID: \(s.address)").font(.caption.monospaced()).foregroundStyle(.secondary)
                Button("Копировать") { store.copy(s.address) }.controlSize(.small)
            }
        }
        .padding(10)
    }
}

struct JoinView: View {
    @EnvironmentObject var store: Store

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            HStack {
                TextField("ID сети (16 символов)", text: $store.joinInput).textFieldStyle(.roundedBorder).font(.body.monospaced())
                    .onSubmit { store.submitJoin() }
                Button("Войти в сеть") { store.submitJoin() }.buttonStyle(.borderedProminent)
            }
            Text("После входа хост должен подтвердить ваш ID на вкладке «Хостинг».")
                .font(.caption).foregroundStyle(.secondary)

            List {
                ForEach(store.networks) { n in NetworkRow(net: n) }
            }
            .frame(minHeight: 140)

            Text("Соединения с участниками").font(.headline)
            PeerTable()
        }
        .padding()
    }
}

struct NetworkRow: View {
    @EnvironmentObject var store: Store
    let net: ZTNetwork
    @AppStorage private var host: String

    init(net: ZTNetwork) {
        self.net = net
        _host = AppStorage(wrappedValue: "", "foundryHost.\(net.id)")
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack {
                VStack(alignment: .leading) {
                    Text(net.name.isEmpty ? "(без имени)" : net.name).font(.headline)
                    Text("\(net.id) · \(statusText) · \(net.ipv4 ?? "нет IP")").font(.caption.monospaced()).foregroundStyle(.secondary)
                }
                Spacer()
                Button("Копировать ID") { store.copy(net.id) }.controlSize(.small)
                Button("Выйти", role: .destructive) { store.leave(net.id) }.controlSize(.small)
            }
            HStack {
                TextField("IP хоста Foundry (например 10.147.20.1)", text: $host).textFieldStyle(.roundedBorder).font(.body.monospaced())
                Button("Открыть Foundry") { openFoundry(host) }.disabled(host.isEmpty)
            }
        }
        .padding(.vertical, 4)
    }

    var statusText: String {
        switch net.status {
        case "OK": return "подключено"
        case "ACCESS_DENIED": return "ждёт подтверждения хоста"
        case "REQUESTING_CONFIGURATION": return "запрос конфигурации…"
        case "NOT_FOUND": return "сеть не найдена"
        default: return net.status
        }
    }

    func openFoundry(_ h: String) {
        let t = h.trimmingCharacters(in: .whitespaces)
        let withPort = t.contains(":") ? t : "\(t):30000"
        guard let url = URL(string: "http://\(withPort)") else { return }
        NSWorkspace.shared.open(url)
    }
}

struct PeerTable: View {
    @EnvironmentObject var store: Store

    var body: some View {
        if store.peers.isEmpty {
            Text("Пока нет активных участников").foregroundStyle(.secondary).frame(maxWidth: .infinity, minHeight: 60)
        } else {
            Table(store.peers) {
                TableColumn("Узел") { p in Text(p.address).monospaced() }
                TableColumn("Тип связи") { p in
                    if p.directPath != nil { Label("Прямая", systemImage: "bolt.fill").foregroundStyle(.green) }
                    else { Label("Через relay (медленно)", systemImage: "exclamationmark.triangle.fill").foregroundStyle(.orange) }
                }
                TableColumn("Пинг") { p in Text(p.latency >= 0 ? "\(p.latency) мс" : "—") }
                TableColumn("Обрывов") { p in Text("\(store.drops[p.address] ?? 0)") }
            }
        }
    }
}

struct HostView: View {
    @EnvironmentObject var store: Store

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            HStack {
                TextField("Название новой сети", text: $store.newName).textFieldStyle(.roundedBorder)
                Button("Создать сеть") { store.submitCreate() }
                    .buttonStyle(.borderedProminent).disabled(store.status == nil)
            }
            Text("Сеть создаётся на встроенном контроллере этого Mac: без внешнего VPS и без лимита по числу устройств.")
                .font(.caption).foregroundStyle(.secondary)
            List {
                ForEach(store.hosted) { n in HostedRow(net: n) }
            }
        }
        .padding()
    }
}

struct HostedRow: View {
    @EnvironmentObject var store: Store
    let net: ZTControllerNetwork

    var body: some View {
        DisclosureGroup {
            ForEach(store.members[net.id] ?? []) { m in
                HStack {
                    Text(m.id).font(.body.monospaced())
                    Text(m.ipAssignments.joined(separator: ", ")).font(.caption.monospaced()).foregroundStyle(.secondary)
                    Spacer()
                    Toggle("Допущен", isOn: Binding(get: { m.authorized }, set: { store.authorize(net.id, m.id, $0) }))
                    Button(role: .destructive) { store.kick(net.id, m.id) } label: { Image(systemName: "trash") }
                }
            }
            if (store.members[net.id] ?? []).isEmpty {
                Text("Участников пока нет").foregroundStyle(.secondary)
            }
        } label: {
            HStack {
                VStack(alignment: .leading) {
                    Text(net.name ?? "(без имени)").font(.headline)
                    Text(net.id).font(.caption.monospaced()).foregroundStyle(.secondary)
                }
                Spacer()
                Button("Копировать ID") { store.copy(net.id) }.controlSize(.small)
                Button("Войти сам") { store.join(net.id) }.controlSize(.small)
                Button("Удалить", role: .destructive) { store.deleteNetwork(net.id) }.controlSize(.small)
            }
        }
    }
}
