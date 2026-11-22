'use strict';

const $ = (id) => document.getElementById(id);
const state = {
  me: null,
  mode: 'login',
  active: { room: 'general', peerId: 0, title: 'Общий чат' },
  users: [],
  online: new Set(),
  unread: new Map(),
  rendered: new Set(),
  socket: null,
  reconnect: 0,
  sequence: 0,
  lastTyping: 0,
  lastDraft: '',
};
let typingTimer = null;
let toastTimer = null;

function toast(message) {
  const el = $('toast');
  el.textContent = message;
  el.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.hidden = true; }, 4500);
}

async function api(path, options = {}) {
  const response = await fetch(path, { credentials: 'same-origin', ...options });
  let body;
  try { body = await response.json(); } catch { body = null; }
  if (!response.ok) {
    const err = new Error(body?.error || `Ошибка сервера (${response.status})`);
    err.status = response.status;
    throw err;
  }
  return body;
}

function selectMode(mode) {
  state.mode = mode;
  $('tab-login').classList.toggle('selected', mode === 'login');
  $('tab-register').classList.toggle('selected', mode === 'register');
  $('auth-password').setAttribute('autocomplete', mode === 'register' ? 'new-password' : 'current-password');
  $('auth-password').minLength = mode === 'register' ? 12 : 1;
  $('auth-submit').textContent = mode === 'register' ? 'Создать аккаунт' : 'Войти в аккаунт';
  $('auth-hint').textContent = mode === 'register'
    ? 'Логин: 3–30 латинских букв, цифр или _. Пароль: не меньше 12 символов.'
    : 'Войди под своим именем и паролем.';
  $('auth-error').textContent = '';
}

async function onAuthSubmit(event) {
  event.preventDefault();
  const username = $('auth-username').value.trim();
  const password = $('auth-password').value;
  const button = $('auth-submit');
  button.disabled = true;
  $('auth-error').textContent = '';
  try {
    state.me = await api(`/api/${state.mode === 'register' ? 'register' : 'login'}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ username, password }),
    });
    $('auth-password').value = '';
    await showApp();
  } catch (err) {
    $('auth-error').textContent = err.message;
  } finally {
    button.disabled = false;
  }
}

function initials(name) { return (name || '?').slice(0, 1).toUpperCase(); }
function friendlyTime(iso) {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleTimeString('ru-RU', { hour: '2-digit', minute: '2-digit' });
}

async function showApp() {
  $('auth-view').hidden = true;
  $('app-view').hidden = false;
  $('my-name').textContent = state.me.username;
  $('my-avatar').textContent = initials(state.me.username);
  state.active = { room: 'general', peerId: 0, title: 'Общий чат' };
  updateActiveHeader();
  await refreshUsers();
  connectSocket();
}
function showAuth() {
  $('app-view').hidden = true;
  $('auth-view').hidden = false;
  state.me = null;
  state.users = [];
  state.online = new Set();
  state.unread.clear();
  state.rendered.clear();
  state.sequence++;
  if (state.socket) { const old = state.socket; state.socket = null; old.close(); }
  selectMode('login');
}

function updateActiveHeader() {
  const a = state.active;
  $('chat-title').textContent = a.title;
  $('chat-avatar').textContent = a.room === 'general' ? '#' : initials(a.title);
  $('chat-avatar').style.background = a.room === 'general' ? '#ce2fce' : '#2563eb';
  const online = state.online.has(a.peerId);
  $('chat-subtitle').textContent = a.room === 'general' ? 'Общая комната · последние 50 сообщений'
    : (online ? 'В сети' : 'Личная переписка');
  $('general-chat').classList.toggle('active', a.room === 'general');
}

function closeSidebar() {
  $('sidebar').classList.remove('open');
  $('sidebar-overlay').hidden = true;
}

function renderUsers() {
  const list = $('user-list');
  list.replaceChildren();
  if (state.users.length === 0) {
    const empty = document.createElement('p');
    empty.className = 'empty-users';
    empty.textContent = 'Других пользователей пока нет. Пригласи друга зарегистрироваться!';
    list.append(empty);
    return;
  }
  for (const u of state.users) {
    const btn = document.createElement('button');
    btn.className = 'chat-entry';
    btn.type = 'button';
    btn.classList.toggle('active', state.active.room === 'direct' && state.active.peerId === u.id);
    const avatar = document.createElement('span');
    avatar.className = 'avatar person-avatar';
    avatar.textContent = initials(u.username);
    const dot = document.createElement('span');
    dot.className = 'dot' + (state.online.has(u.id) ? ' online' : '');
    avatar.append(dot);
    const main = document.createElement('span');
    main.className = 'chat-entry-main';
    const name = document.createElement('strong'); name.textContent = u.username;
    const small = document.createElement('small'); small.textContent = state.online.has(u.id) ? 'В сети' : 'Не в сети';
    main.append(name, small);
    btn.append(avatar, main);
    const unread = state.unread.get(u.id) || 0;
    if (unread) {
      const badge = document.createElement('span'); badge.className = 'unread-badge';
      badge.textContent = unread > 99 ? '99+' : String(unread);
      btn.append(badge);
    }
    btn.addEventListener('click', () => {
      state.unread.delete(u.id);
      setActive({ room: 'direct', peerId: u.id, title: u.username });
    });
    list.append(btn);
  }
}

async function refreshUsers() {
  if (!state.me) return;
  try {
    state.users = await api('/api/users');
    renderUsers();
  } catch (err) {
    if (err.status === 401) showAuth();
    else toast('Не удалось обновить список пользователей');
  }
}

function clearHistory() {
  state.rendered.clear();
  $('messages').replaceChildren();
  $('typing-indicator').textContent = '';
}
function setActive(next) {
  state.active = next;
  state.sequence++;
  clearHistory();
  updateActiveHeader();
  renderUsers();
  closeSidebar();
  void loadHistory();
  $('message-input').focus();
}

function renderMessage(m) {
  const a = state.active;
  if (m.room !== a.room) return;
  if (a.room === 'direct' && !((m.sender_id === state.me.id && m.peer_id === a.peerId)
    || (m.sender_id === a.peerId && m.peer_id === state.me.id))) return;
  const key = `${m.room}:${m.id}`;
  if (state.rendered.has(key)) return;
  state.rendered.add(key);
  const el = document.createElement('div');
  const mine = m.sender_id === state.me.id;
  el.className = 'message' + (mine ? ' mine' : '');
  el.dataset.id = String(m.id);
  const meta = document.createElement('div'); meta.className = 'message-meta';
  const name = document.createElement('strong'); name.textContent = mine ? 'Вы' : m.sender;
  const date = document.createElement('time');
  date.dateTime = m.created_at;
  date.textContent = friendlyTime(m.created_at);
  const body = document.createElement('div'); body.className = 'message-body';
  body.textContent = m.text; // Never interpret chat text as HTML.
  meta.append(name, date); el.append(meta, body);
  const messages = $('messages');
  const old = [...messages.querySelectorAll('.message')]
    .find(node => Number(node.dataset.id) > Number(m.id));
  messages.insertBefore(el, old || null);
  messages.scrollTop = messages.scrollHeight;
}

function showEmptyHistory() {
  if ($('messages').children.length !== 0) return;
  const div = document.createElement('div'); div.className = 'chat-empty';
  const icon = document.createElement('span'); icon.className = 'chat-empty-symbol'; icon.textContent = '♡';
  const label = document.createElement('span'); label.textContent = 'Здесь пока тихо. Отправь первое сообщение!';
  div.append(icon, label); $('messages').append(div);
}

async function loadHistory() {
  const sequence = ++state.sequence;
  const query = state.active.room === 'direct' ? `?peer_id=${state.active.peerId}` : '';
  try {
    const history = await api('/api/messages' + query);
    if (sequence !== state.sequence || !state.me) return;
    $('messages').querySelectorAll('.chat-empty').forEach(el => el.remove());
    for (const msg of history) renderMessage(msg);
    showEmptyHistory();
  } catch (err) {
    if (sequence === state.sequence) {
      if (err.status === 401) showAuth(); else toast('Не удалось загрузить историю');
    }
  }
}

function setConnection(online) {
  $('connection').classList.toggle('connected', online);
  $('connection-label').textContent = online ? 'На связи' : 'Нет соединения';
  $('send').disabled = !online;
}

function connectSocket() {
  if (!state.me || (state.socket && state.socket.readyState === WebSocket.OPEN)) return;
  if (state.socket && state.socket.readyState === WebSocket.CONNECTING) return;
  const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  const ws = new WebSocket(`${protocol}//${window.location.host}/ws`);
  state.socket = ws;
  setConnection(false);
  ws.onopen = () => {
    if (state.socket !== ws) return;
    state.reconnect = 0;
    setConnection(true);
    void loadHistory(); // Covers messages missed during reconnection.
  };
  ws.onmessage = (event) => {
    let data;
    try { data = JSON.parse(event.data); } catch { return; }
    switch (data.type) {
      case 'message': {
        const m = data.message;
        if (!m) break;
        if (m.room === 'direct' && m.sender_id !== state.me.id &&
            !(state.active.room === 'direct' && state.active.peerId === m.sender_id)) {
          state.unread.set(m.sender_id, (state.unread.get(m.sender_id) || 0) + 1);
          renderUsers();
          toast(`Новое сообщение от ${m.sender}`);
        } else if (m.room === state.active.room) {
          $('messages').querySelectorAll('.chat-empty').forEach(el => el.remove());
          renderMessage(m);
        }
        break;
      }
      case 'online_snapshot': state.online = new Set(data.users || []); renderUsers(); updateActiveHeader(); break;
      case 'presence': data.online ? state.online.add(data.user_id) : state.online.delete(data.user_id);
        renderUsers(); updateActiveHeader(); break;
      case 'typing':
        if (data.user_id !== state.me.id && data.room === state.active.room &&
           (data.room === 'general' || data.user_id === state.active.peerId)) {
          $('typing-indicator').textContent = `${data.sender} печатает…`;
          clearTimeout(typingTimer);
          typingTimer = setTimeout(() => { $('typing-indicator').textContent = ''; }, 1800);
        }
        break;
      case 'error': toast(data.text || 'Ошибка отправки');
        if (state.lastDraft && !$('message-input').value) $('message-input').value = state.lastDraft;
        break;
    }
  };
  ws.onclose = () => {
    if (state.socket !== ws) return;
    state.socket = null;
    setConnection(false);
    if (state.me) {
      const wait = Math.min(1000 * 2 ** state.reconnect++, 12000);
      setTimeout(() => { if (state.me && !state.socket) connectSocket(); }, wait);
    }
  };
  ws.onerror = () => { /* onclose handles reconnect */ };
}

function sendMessage(event) {
  event.preventDefault();
  const text = $('message-input').value.trim();
  const ws = state.socket;
  if (!text || !ws || ws.readyState !== WebSocket.OPEN) return;
  state.lastDraft = text;
  ws.send(JSON.stringify({ type: 'message', room: state.active.room,
    peer_id: state.active.peerId, text }));
  $('message-input').value = '';
  $('message-input').focus();
}
function notifyTyping() {
  const now = Date.now();
  if (now - state.lastTyping < 1300) return;
  const ws = state.socket;
  if (!ws || ws.readyState !== WebSocket.OPEN) return;
  state.lastTyping = now;
  ws.send(JSON.stringify({ type: 'typing', room: state.active.room, peer_id: state.active.peerId }));
}
async function logout() {
  try { await api('/api/logout', { method: 'POST' }); }
  catch (err) { if (err.status !== 401) { toast('Не удалось выйти из аккаунта'); return; } }
  showAuth();
}

async function init() {
  $('tab-login').addEventListener('click', () => selectMode('login'));
  $('tab-register').addEventListener('click', () => selectMode('register'));
  $('auth-form').addEventListener('submit', onAuthSubmit);
  $('logout').addEventListener('click', logout);
  $('send-form').addEventListener('submit', sendMessage);
  $('message-input').addEventListener('input', notifyTyping);
  $('general-chat').addEventListener('click', () => setActive({ room: 'general', peerId: 0, title: 'Общий чат' }));
  $('refresh-users').addEventListener('click', () => void refreshUsers());
  $('menu-toggle').addEventListener('click', () => {
    $('sidebar').classList.add('open'); $('sidebar-overlay').hidden = false;
  });
  $('sidebar-overlay').addEventListener('click', closeSidebar);
  selectMode('login');
  try {
    state.me = await api('/api/me');
    await showApp();
  } catch (err) {
    if (err.status !== 401) toast('Сервер недоступен — попробуй обновить страницу');
    showAuth();
  }
  setInterval(() => { if (state.me) void refreshUsers(); }, 30000);
}

void init();
