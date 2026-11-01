import Foundation

struct ZTError: LocalizedError {
    let message: String
    var errorDescription: String? { message }
}

struct ZTStatus: Decodable {
    let address: String
    let online: Bool
    let version: String
}

struct ZTNetwork: Decodable, Identifiable {
    let id: String
    let name: String
    let status: String
    let type: String
    let assignedAddresses: [String]
    let portDeviceName: String?

    var ipv4: String? {
        assignedAddresses.first { $0.contains(".") }?.components(separatedBy: "/").first
    }
}

struct ZTPeer: Decodable, Identifiable {
    struct Path: Decodable {
        let active: Bool
        let expired: Bool
        let preferred: Bool
        let address: String
    }
    let address: String
    let latency: Int
    let role: String
    let version: String
    let paths: [Path]
    var id: String { address }

    var directPath: Path? { paths.first { $0.active && !$0.expired && $0.preferred } ?? paths.first { $0.active && !$0.expired } }
}

struct ZTMember: Decodable, Identifiable {
    let id: String
    let nwid: String
    let authorized: Bool
    let ipAssignments: [String]
}

struct ZTControllerNetwork: Decodable, Identifiable {
    let id: String
    let name: String?
    let `private`: Bool?
}

/// Клиент локального API службы ZeroTier One (127.0.0.1:9993).
/// Токен берётся из пользовательской копии authtoken.secret, права root не нужны.
final class ZTAPI {
    private let base = URL(string: "http://127.0.0.1:9993")!
    private let session: URLSession

    init() {
        let cfg = URLSessionConfiguration.ephemeral
        cfg.timeoutIntervalForRequest = 4
        session = URLSession(configuration: cfg)
    }

    private func token() throws -> String {
        let fm = FileManager.default
        let home = fm.homeDirectoryForCurrentUser.path
        let paths = [
            "\(home)/Library/Application Support/ZeroTier/One/authtoken.secret",
            "\(home)/Library/Application Support/ZeroTier/authtoken.secret",
            "/Library/Application Support/ZeroTier/One/authtoken.secret",
        ]
        for p in paths {
            if let s = try? String(contentsOfFile: p, encoding: .utf8) {
                let t = s.trimmingCharacters(in: .whitespacesAndNewlines)
                if !t.isEmpty { return t }
            }
        }
        throw ZTError(message: "Не найден authtoken.secret. Установите ZeroTier One и запустите его.")
    }

    @discardableResult
    func send(_ method: String, _ path: String, body: [String: Any]? = nil) async throws -> Data {
        var req = URLRequest(url: base.appendingPathComponent(path))
        req.httpMethod = method
        req.setValue(try token(), forHTTPHeaderField: "X-ZT1-Auth")
        if let body {
            req.httpBody = try JSONSerialization.data(withJSONObject: body)
            req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        }
        let data: Data, resp: URLResponse
        do { (data, resp) = try await session.data(for: req) }
        catch { throw ZTError(message: "Служба ZeroTier не отвечает. Запущен ли ZeroTier One?") }
        if let http = resp as? HTTPURLResponse, !(200..<300).contains(http.statusCode) {
            throw ZTError(message: "ZeroTier API \(http.statusCode) на \(method) \(path)")
        }
        return data
    }

    private func get<T: Decodable>(_ path: String, as: T.Type = T.self) async throws -> T {
        try JSONDecoder().decode(T.self, from: try await send("GET", path))
    }

    func status() async throws -> ZTStatus { try await get("status") }
    func networks() async throws -> [ZTNetwork] { try await get("network") }
    func peers() async throws -> [ZTPeer] { try await get("peer") }

    func join(_ nwid: String) async throws { try await send("POST", "network/\(nwid)", body: [:]) }
    func leave(_ nwid: String) async throws { try await send("DELETE", "network/\(nwid)") }

    // Контроллер (встроенный в службу на машине-хосте)
    func controllerNetworks() async throws -> [ZTControllerNetwork] {
        let ids: [String] = try await get("controller/network")
        var out: [ZTControllerNetwork] = []
        for id in ids {
            if let n = try? await get("controller/network/\(id)", as: ZTControllerNetwork.self) { out.append(n) }
        }
        return out
    }

    func createNetwork(name: String, nodeId: String) async throws {
        let octet = Int.random(in: 16...250)
        let body: [String: Any] = [
            "name": name,
            "private": true,
            "v4AssignMode": ["zt": true],
            "ipAssignmentPools": [["ipRangeStart": "10.147.\(octet).1", "ipRangeEnd": "10.147.\(octet).254"]],
            "routes": [["target": "10.147.\(octet).0/24"]],
        ]
        try await send("POST", "controller/network/\(nodeId)______", body: body)
    }

    func deleteNetwork(_ nwid: String) async throws { try await send("DELETE", "controller/network/\(nwid)") }

    func members(of nwid: String) async throws -> [ZTMember] {
        let map: [String: Int] = try await get("controller/network/\(nwid)/member")
        var out: [ZTMember] = []
        for id in map.keys.sorted() {
            if let m = try? await get("controller/network/\(nwid)/member/\(id)", as: ZTMember.self) { out.append(m) }
        }
        return out
    }

    func setAuthorized(_ nwid: String, member: String, _ value: Bool) async throws {
        try await send("POST", "controller/network/\(nwid)/member/\(member)", body: ["authorized": value])
    }

    func removeMember(_ nwid: String, member: String) async throws {
        try await send("DELETE", "controller/network/\(nwid)/member/\(member)")
    }
}
