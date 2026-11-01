using System.Net.Http;
using System.Text;
using System.Text.Json;

namespace FoundryLink;

/// Клиент локального API службы ZeroTier One (127.0.0.1:9993).
public sealed class ZtApi
{
    private static readonly string TokenPath =
        Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.CommonApplicationData), "ZeroTier", "One", "authtoken.secret");

    private readonly HttpClient _http = new() { BaseAddress = new Uri("http://127.0.0.1:9993/"), Timeout = TimeSpan.FromSeconds(4) };

    private async Task<JsonElement> Send(HttpMethod m, string path, object? body = null)
    {
        if (!File.Exists(TokenPath))
            throw new InvalidOperationException("Не найден authtoken.secret. Установите ZeroTier One.");
        using var req = new HttpRequestMessage(m, path);
        req.Headers.Add("X-ZT1-Auth", (await File.ReadAllTextAsync(TokenPath)).Trim());
        if (body != null) req.Content = new StringContent(JsonSerializer.Serialize(body), Encoding.UTF8, "application/json");
        try
        {
            using var resp = await _http.SendAsync(req);
            var text = await resp.Content.ReadAsStringAsync();
            if (!resp.IsSuccessStatusCode) throw new InvalidOperationException($"ZeroTier API {(int)resp.StatusCode} на {path}");
            return text.Length == 0 ? default : JsonDocument.Parse(text).RootElement.Clone();
        }
        catch (HttpRequestException) { throw new InvalidOperationException("Служба ZeroTier не отвечает. Запущен ли ZeroTier One?"); }
        catch (TaskCanceledException) { throw new InvalidOperationException("Служба ZeroTier не отвечает."); }
    }

    public async Task<(string Address, bool Online)> Status()
    {
        var j = await Send(HttpMethod.Get, "status");
        return (j.GetProperty("address").GetString() ?? "", j.GetProperty("online").GetBoolean());
    }

    public async Task<List<NetInfo>> Networks()
    {
        var j = await Send(HttpMethod.Get, "network");
        return j.EnumerateArray().Select(n => new NetInfo(
            n.GetProperty("id").GetString() ?? "",
            n.GetProperty("name").GetString() ?? "",
            n.GetProperty("status").GetString() ?? "",
            n.GetProperty("assignedAddresses").EnumerateArray()
                .Select(a => a.GetString() ?? "").FirstOrDefault(a => a.Contains('.'))?.Split('/')[0] ?? "")).ToList();
    }

    public async Task<List<PeerInfo>> Peers()
    {
        var j = await Send(HttpMethod.Get, "peer");
        return j.EnumerateArray().Where(p => p.GetProperty("role").GetString() == "LEAF").Select(p => new PeerInfo(
            p.GetProperty("address").GetString() ?? "",
            p.GetProperty("latency").GetInt32(),
            p.GetProperty("paths").EnumerateArray().Any(x => x.GetProperty("active").GetBoolean() && !x.GetProperty("expired").GetBoolean()))).ToList();
    }

    public Task Join(string nwid) => Send(HttpMethod.Post, $"network/{nwid}", new { });
    public Task Leave(string nwid) => Send(HttpMethod.Delete, $"network/{nwid}");
}

public record NetInfo(string Id, string Name, string Status, string Ip)
{
    public string StatusText => Status switch
    {
        "OK" => "подключено",
        "ACCESS_DENIED" => "ждёт подтверждения хоста",
        "REQUESTING_CONFIGURATION" => "запрос конфигурации…",
        "NOT_FOUND" => "сеть не найдена",
        _ => Status,
    };
    public string Line => $"{Id} · {StatusText} · {(Ip == "" ? "нет IP" : Ip)}";
}

public record PeerInfo(string Address, int Latency, bool Direct)
{
    public string Kind => Direct ? "Прямая" : "Через relay (медленно)";
    public string Ping => Latency >= 0 ? $"{Latency} мс" : "—";
}
