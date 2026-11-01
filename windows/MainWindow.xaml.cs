using System.Windows;
using System.Windows.Controls;
using System.Windows.Threading;

namespace FoundryLink;

public partial class MainWindow : Window
{
    private readonly ZtApi _api = new();
    private readonly DispatcherTimer _timer = new() { Interval = TimeSpan.FromSeconds(2) };
    private bool _busy;

    public MainWindow()
    {
        InitializeComponent();
        _timer.Tick += async (_, _) => await Refresh();
        _timer.Start();
        Loaded += async (_, _) => await Refresh();
    }

    private async Task Refresh()
    {
        if (_busy) return;
        _busy = true;
        try
        {
            var (addr, online) = await _api.Status();
            StatusText.Text = $"{(online ? "В сети" : "Нет связи с корневыми серверами")} · мой ID: {addr}";
            NetList.ItemsSource = await _api.Networks();
            PeerList.ItemsSource = await _api.Peers();
            ErrorText.Visibility = Visibility.Collapsed;
        }
        catch (Exception e)
        {
            StatusText.Text = "Служба недоступна";
            ShowError(e.Message);
        }
        finally { _busy = false; }
    }

    private void ShowError(string msg)
    {
        ErrorText.Text = msg;
        ErrorText.Visibility = Visibility.Visible;
    }

    private async void Join_Click(object sender, RoutedEventArgs e)
    {
        var id = NwidBox.Text.Trim().ToLowerInvariant();
        if (id.Length != 16 || !id.All(Uri.IsHexDigit)) { ShowError("ID сети — 16 символов (0-9, a-f)"); return; }
        try { await _api.Join(id); NwidBox.Clear(); await Refresh(); }
        catch (Exception ex) { ShowError(ex.Message); }
    }

    private async void Leave_Click(object sender, RoutedEventArgs e)
    {
        if (((Button)sender).Tag is not string id) return;
        try { await _api.Leave(id); await Refresh(); }
        catch (Exception ex) { ShowError(ex.Message); }
    }
}
