'use strict';
const TOKEN = document.querySelector('meta[name=fl-token]').content;
const $ = (s) => document.querySelector(s);
let S = null;          // последнее состояние
let quitting = false;  // пользователь нажал «Выйти»
let busy = false;      // идёт запрос действия

// ---------- утилиты ----------
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const fmtBytes = (n) => {
  if (n < 1024) return n + ' Б';
  const u = ['КБ', 'МБ', 'ГБ', 'ТБ']; let i = -1;
  do { n /= 1024; i++; } while (n >= 1024 && i < u.length - 1);
  return n.toFixed(n < 10 ? 1 : 0) + ' ' + u[i];
};
const fmtDur = (since) => {
  if (!since) return '—';
  let s = Math.max(0, Math.floor(Date.now() / 1000 - since));
  const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60); s %= 60;
  return (h ? h + ':' + String(m).padStart(2, '0') : m) + ':' + String(s).padStart(2, '0');
};
const fmtMs = (v) => (v < 10 ? v.toFixed(1) : Math.round(v));

function toast(msg, err) {
  const t = $('#toast');
  t.textContent = msg; t.className = 'show' + (err ? ' err' : '');
  clearTimeout(toast.t); toast.t = setTimeout(() => (t.className = ''), 3500);
}

async function api(path, body) {
  busy = true;
  try {
    const r = await fetch('/api/' + path, {
      method: 'POST', headers: { 'Content-Type': 'application/json', 'X-FL-Token': TOKEN },
      body: JSON.stringify(body || {}),
    });
    const j = await r.json();
    if (j.state) render(j.state);
    return j;
  } catch (e) {
    if (!quitting) toast('Нет связи с приложением', true);
    return { ok: false, error: String(e) };
  } finally { busy = false; }
}

async function copy(text, okMsg) {
  try { await navigator.clipboard.writeText(text); }
  catch {
    const ta = document.createElement('textarea'); ta.value = text; document.body.appendChild(ta);
    ta.select(); document.execCommand('copy'); ta.remove();
  }
  toast(okMsg || 'Скопировано');
}

function setIfIdle(el, value) { if (document.activeElement !== el && el.value !== String(value)) el.value = value; }

// ---------- график пинга ----------
const Q_COLOR = { excellent: '--good', good: '--ok', fair: '--warn', poor: '--bad', none: '--muted' };
function drawSpark(cv, hist, quality) {
  const dpr = window.devicePixelRatio || 1;
  const w = cv.clientWidth, h = cv.clientHeight;
  if (!w || !h) return;
  cv.width = w * dpr; cv.height = h * dpr;
  const g = cv.getContext('2d'); g.scale(dpr, dpr); g.clearRect(0, 0, w, h);
  const css = getComputedStyle(document.documentElement);
  const col = css.getPropertyValue(Q_COLOR[quality] || '--muted').trim();
  const bad = css.getPropertyValue('--bad').trim();
  const pts = hist || [];
  if (!pts.length) return;
  const vals = pts.filter((v) => v >= 0);
  const max = Math.max(60, ...vals) * 1.15;
  const step = w / Math.max(pts.length - 1, 1);
  g.lineWidth = 2; g.lineJoin = 'round'; g.strokeStyle = col; g.beginPath();
  let started = false;
  pts.forEach((v, i) => {
    if (v < 0) { started = false; return; }
    const x = i * step, y = h - 3 - (v / max) * (h - 8);
    if (!started) { g.moveTo(x, y); started = true; } else g.lineTo(x, y);
  });
  g.stroke();
  g.fillStyle = bad;
  pts.forEach((v, i) => { if (v < 0) g.fillRect(i * step - 1, h - 6, 2.5, 6); });
}

function drawAllSparks() {
  document.querySelectorAll('canvas[data-hist]').forEach((cv) => {
    try { drawSpark(cv, JSON.parse(cv.dataset.hist), cv.dataset.q); } catch { /* ignore */ }
  });
}

// ---------- отрисовка ----------
function show(id) {
  ['v-choose', 'v-host', 'v-player'].forEach((v) => ($('#' + v).hidden = v !== id));
}

function render(s) {
  S = s;
  $('#ver').textContent = 'v' + s.version;
  $('#update-bar').hidden = !s.update;
  if (s.update) { $('#update-ver').textContent = 'v' + s.update.version; $('#update-link').href = s.update.url; }
  $('#btn-role').hidden = !s.mode;
  $('#log-card').hidden = !s.mode;
  $('#log').textContent = (s.log || []).join('\n');
  if (s.mode === 'host') { show('v-host'); renderHost(s); }
  else if (s.mode === 'player') { show('v-player'); renderPlayer(s); }
  else show('v-choose');
  drawAllSparks();
}

function tile(id, level, value, sub) {
  const t = $('#' + id); t.className = 'tile ' + (level || '');
  t.querySelector('.v').textContent = value; t.querySelector('.s').textContent = sub || '';
}

function renderHost(s) {
  const h = s.host;
  $('#host-stopped').hidden = !!h;
  $('#host-main').hidden = !h;
  setIfIdle($('#s-hostname'), s.settings.hostName);
  setIfIdle($('#s-fport'), s.settings.foundryPort);
  if (!h) return;

  if (h.foundryUp) tile('t-foundry', 'good', 'Работает', 'localhost:' + s.settings.foundryPort);
  else tile('t-foundry', 'bad', 'Не найден', 'Запустите Foundry на порту ' + s.settings.foundryPort);

  const nat = s.nat;
  if (nat.ready) tile('t-net', nat.level, nat.level === 'good' ? 'Отлично' : nat.level === 'warn' ? 'Есть оговорки' : 'Проблема',
    [nat.v4, nat.v6].filter(Boolean).join('  '));
  else tile('t-net', '', 'Определяю…', 'проверяю внешний адрес');
  $('#nat-warn').hidden = !(nat.ready && nat.level !== 'good');
  $('#nat-warn').className = 'notice ' + (nat.level === 'bad' ? 'bad' : 'warn');
  $('#nat-warn').textContent = nat.text || '';

  tile('t-online', h.online ? 'good' : '', h.online + ' онлайн', 'в списке: ' + h.players.length + ' из ' + s.maxPlayers);
  $('#host-code').textContent = h.code || 'Определяю адрес…';
  $('#btn-copy-host').disabled = !h.code;
  $('#players-count').textContent = h.players.length ? `${h.players.length} из ${s.maxPlayers}` : '';

  const box = $('#players');
  if (!h.players.length) {
    box.innerHTML = '<p class="hint">Пока никого. Отправьте игрокам код комнаты и добавьте их ответные коды выше.</p>';
    return;
  }
  box.innerHTML = h.players.map((p) => {
    const st = p.stats;
    const online = p.state === 'online';
    const head = `<div class="pl-head"><span class="dot"></span><b>${esc(p.name)}</b>
      <span class="muted">#${esc(p.short)}${online ? ' · ' + esc(p.path) + ' · ' + fmtDur(p.since) : ''}</span>
      ${online ? `<span class="badge q-${st.quality}">${esc(st.qualityText)}</span>` : ''}
      <button class="ghost danger" data-act="host-remove" data-id="${esc(p.id)}">Удалить</button></div>`;
    if (!online) return `<div class="pl">${head}<div class="pl-wait">Ожидает подключения… Мастер и игрок «стучатся» друг к другу автоматически.</div></div>`;
    return `<div class="pl online">${head}<div class="pl-body">
      <div class="ping"><span class="num q-${st.quality}">${st.have ? fmtMs(st.ping) : '—'}</span><span class="unit">мс</span></div>
      <canvas class="spark" data-q="${st.quality}" data-hist='${JSON.stringify(st.history || [])}'></canvas></div>
      <div class="metrics"><span>Джиттер <b>${fmtMs(st.jitter)} мс</b></span><span>Потери <b>${st.loss}%</b></span>
      <span>Мин. <b>${fmtMs(st.min)} мс</b></span><span>Переподключений <b>${p.reconnects}</b></span>
      <span>↑ <b>${fmtBytes(p.sent)}</b> ↓ <b>${fmtBytes(p.recv)}</b></span></div></div>`;
  }).join('');
}

function renderPlayer(s) {
  const p = s.player;
  $('#join-card').hidden = !!p;
  $('#player-main').hidden = !p;
  setIfIdle($('#s-lport'), s.settings.localPort);
  if (!p) {
    const n = $('#p-name'), r = $('#p-room');
    if (document.activeElement !== n && !n.value) n.value = s.settings.playerName || '';
    if (document.activeElement !== r && !r.value) r.value = s.settings.roomCode || '';
    return;
  }
  const online = p.state === 'online';
  const st = p.stats;
  $('#hero').className = 'card hero' + (online ? '' : ' waiting');
  const states = {
    online: ['Подключено', p.hostName ? 'Мастер: ' + p.hostName + ' · канал ' + p.path : 'канал ' + p.path],
    connecting: ['Подключаюсь…', p.message || 'Пробиваю соединение с мастером'],
    reconnecting: ['Переподключаюсь…', p.message || 'Связь потеряна, восстанавливаю'],
    notadded: ['Ждём мастера', p.message || 'Мастер ещё не добавил ваш ответный код'],
    error: ['Ошибка', p.message],
  };
  const [title, sub] = states[p.state] || ['…', ''];
  $('#p-state').textContent = title; $('#p-sub').textContent = sub;
  const badge = $('#p-badge');
  badge.hidden = !online; badge.className = 'badge q-' + st.quality; badge.textContent = st.qualityText;
  const ping = $('#p-ping');
  ping.textContent = online && st.have ? fmtMs(st.ping) : '—'; ping.className = 'num q-' + (online ? st.quality : 'none');
  const cv = $('#p-spark'); cv.dataset.hist = JSON.stringify(online ? st.history || [] : []); cv.dataset.q = st.quality;
  cv.setAttribute('data-hist', cv.dataset.hist);
  $('#p-metrics').innerHTML = online
    ? `<span>Джиттер <b>${fmtMs(st.jitter)} мс</b></span><span>Потери <b>${st.loss}%</b></span><span>Мин. <b>${fmtMs(st.min)} мс</b></span>
       <span>В сети <b>${fmtDur(p.since)}</b></span><span>Переподключений <b>${p.reconnects}</b></span>
       <span>↑ <b>${fmtBytes(p.sent)}</b> ↓ <b>${fmtBytes(p.recv)}</b></span>`
    : (p.reconnects ? `<span>Переподключений: <b>${p.reconnects}</b></span>` : '');
  const open = $('#p-open');
  open.href = p.localUrl; open.textContent = 'Открыть Foundry';
  open.style.opacity = online ? '1' : '.5';
  open.style.pointerEvents = online ? 'auto' : 'none';
  open.title = p.localUrl;

  $('#reply-code').textContent = p.replyCode || 'Определяю адрес…';
  $('#btn-copy-reply').disabled = !p.replyCode;
  const rc = $('#reply-card');
  rc.style.borderColor = p.state === 'notadded' ? 'var(--accent)' : '';
  $('#reply-hint').textContent = online
    ? 'Мастеру код уже не нужен. Он понадобится, если ваш адрес изменится и нужно будет подключиться заново.'
    : 'Отправьте этот код мастеру. Пока он не вставит его у себя, подключиться нельзя.';
}

// ---------- действия ----------
document.addEventListener('click', async (e) => {
  const b = e.target.closest('[data-act]');
  if (!b || busy) return;
  const act = b.dataset.act;
  switch (act) {
    case 'role-host': await api('mode', { mode: 'host' }); break;
    case 'role-player': await api('mode', { mode: 'player' }); break;
    case 'change-role': {
      const active = (S.host && S.host.online) || (S.player && S.player.state === 'online');
      if (active && !confirm('Сейчас есть активное подключение. Сменить роль и отключиться?')) return;
      await api('mode', { mode: '' }); break;
    }
    case 'quit':
      if (confirm('Выйти из Foundry Link? Все подключения будут разорваны.')) {
        quitting = true;
        await api('quit');
        document.body.innerHTML = '<p style="font:16px system-ui;text-align:center;margin:20vh 16px;color:#98a0b3">Foundry Link закрыт. Это окно можно закрыть.</p>';
      }
      break;
    case 'copy-log': {
      const txt = (S && S.log || []).join('\n');
      if (txt) copy(txt, 'Журнал скопирован'); else toast('Журнал пуст', true);
      break;
    }
    case 'copy-host-code': if (S.host?.code) copy(S.host.code, 'Код комнаты скопирован'); break;
    case 'copy-reply': if (S.player?.replyCode) copy(S.player.replyCode, 'Ответный код скопирован'); break;
    case 'copy-local': if (S.player) copy(S.player.localUrl, 'Адрес скопирован'); break;
    case 'host-start': { const r = await api('host/start'); if (!r.ok) toast(r.error, true); break; }
    case 'host-stop': if (confirm('Остановить хост? Игроки будут отключены.')) await api('host/stop'); break;
    case 'host-add': {
      const ta = $('#add-text'), msg = $('#add-msg');
      const r = await api('host/add', { codes: ta.value });
      if (!r.ok) { msg.className = 'msg err'; msg.textContent = r.error; break; }
      ta.value = '';
      const errs = (r.extra && r.extra.errors) || [];
      msg.className = errs.length ? 'msg err' : 'msg ok';
      msg.textContent = 'Добавлено: ' + r.extra.added + (errs.length ? '. Ошибки: ' + errs.join('; ') : '');
      break;
    }
    case 'host-remove': {
      const name = b.closest('.pl').querySelector('b').textContent;
      if (confirm('Удалить игрока «' + name + '»? Он больше не сможет подключиться.')) await api('host/remove', { id: b.dataset.id });
      break;
    }
    case 'host-save': {
      const r = await api('host/settings', { name: $('#s-hostname').value, foundryPort: parseInt($('#s-fport').value, 10) });
      toast(r.ok ? 'Сохранено' : r.error, !r.ok); break;
    }
    case 'player-start': {
      const msg = $('#join-msg');
      const r = await api('player/start', { code: $('#p-room').value, name: $('#p-name').value });
      msg.className = 'msg err'; msg.textContent = r.ok ? '' : r.error; break;
    }
    case 'player-stop': await api('player/stop'); break;
    case 'player-save': {
      const r = await api('player/settings', { localPort: parseInt($('#s-lport').value, 10) });
      toast(r.ok ? 'Сохранено' : r.error, !r.ok); break;
    }
  }
});

// ---------- живые обновления ----------
function connect() {
  const es = new EventSource('/api/events?t=' + TOKEN);
  es.onmessage = (m) => { $('#conn-lost').hidden = true; if (!busy) render(JSON.parse(m.data)); };
  es.onerror = () => { if (!quitting) $('#conn-lost').hidden = false; };
}
connect();
window.addEventListener('resize', drawAllSparks);
