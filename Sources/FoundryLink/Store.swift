import SwiftUI
import AppKit

@MainActor
final class Store: ObservableObject {
    @Published var status: ZTStatus?
    @Published var networks: [ZTNetwork] = []
    @Published var peers: [ZTPeer] = []
    @Published var hosted: [ZTControllerNetwork] = []
    @Published var members: [String: [ZTMember]] = [:]
    @Published var error: String?
    @Published var drops: [String: Int] = [:]
    @Published var joinInput = ""
    @Published var newName = ""

    private let api = ZTAPI()
    private var wasDirect: [String: Bool] = [:]
    private var loop: Task<Void, Never>?

    func start() {
        guard loop == nil else { return }
        loop = Task { [weak self] in
            while !Task.isCancelled {
                await self?.refresh()
                try? await Task.sleep(for: .seconds(2))
            }
        }
    }

    func refresh() async {
        do {
            status = try await api.status()
            networks = try await api.networks().sorted { $0.name < $1.name }
            let ps = try await api.peers().filter { $0.role == "LEAF" }
            trackDrops(ps)
            peers = ps.sorted { $0.address < $1.address }
            error = nil
        } catch {
            self.error = error.localizedDescription
            status = nil
            return
        }
        // Контроллер есть не у всех; ошибки здесь не критичны
        if let h = try? await api.controllerNetworks() {
            hosted = h
            for n in h { members[n.id] = (try? await api.members(of: n.id)) ?? members[n.id] }
        }
    }

    private func trackDrops(_ ps: [ZTPeer]) {
        for p in ps {
            let now = p.directPath != nil
            if wasDirect[p.address] == true && !now { drops[p.address, default: 0] += 1 }
            wasDirect[p.address] = now
        }
    }

    func run(_ op: @escaping () async throws -> Void) {
        Task {
            do { try await op() } catch { self.error = error.localizedDescription }
            await refresh()
        }
    }

    @discardableResult
    func join(_ raw: String) -> Bool {
        let id = raw.trimmingCharacters(in: .whitespacesAndNewlines).lowercased()
        guard id.count == 16, id.allSatisfy(\.isHexDigit) else {
            error = "ID сети — 16 символов (0-9, a-f)"
            return false
        }
        run { try await self.api.join(id) }
        return true
    }

    func submitJoin() { if join(joinInput) { joinInput = "" } }

    func submitCreate() {
        createNetwork(newName.isEmpty ? "Foundry" : newName)
        newName = ""
    }

    func leave(_ id: String) { run { try await self.api.leave(id) } }

    func createNetwork(_ name: String) {
        guard let node = status?.address else { return }
        run { try await self.api.createNetwork(name: name, nodeId: node) }
    }

    func deleteNetwork(_ id: String) { run { try await self.api.deleteNetwork(id) } }
    func authorize(_ nwid: String, _ member: String, _ v: Bool) { run { try await self.api.setAuthorized(nwid, member: member, v) } }
    func kick(_ nwid: String, _ member: String) { run { try await self.api.removeMember(nwid, member: member) } }

    func copy(_ s: String) {
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(s, forType: .string)
    }
}
