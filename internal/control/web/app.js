'use strict';
const $ = id => document.getElementById(id);
const node = (tag, text, cls) => { const el = document.createElement(tag); if (text !== undefined) el.textContent = text; if (cls) el.className = cls; return el; };
let token = sessionStorage.getItem('gateway-token') || '';
let state = {executors: [], sessions: [], downloads: {}, model_ready: false};
let selected = sessionStorage.getItem('gateway-session') || '', detail = null;
let loading = false, submitting = false, connected = false, lastTimeline = '';
const modeNames = {ready: 'Executor 在线', chat_only: '仅聊天', busy: 'Executor 忙碌'};
async function api(path, method = 'GET', body) {
  const response = await fetch(path, {method, headers: {'Authorization': `Bearer ${token}`, 'Content-Type': 'application/json'}, body: body === undefined ? undefined : JSON.stringify(body), signal: AbortSignal.timeout(10000)});
  const data = await response.json();
  if (!response.ok) { const err = new Error(data.error || `请求失败 (${response.status})`); err.status = response.status; throw err; }
  return data;
}
function showLogin() { $('login').hidden = false; $('console').hidden = true; }
function showError(message) { $('page-error').hidden = false; $('page-error').textContent = message; }
async function refresh() {
  if (!token || loading) return;
  loading = true;
  const auth = token;
  try {
    const next = await api('/api/state');
    if (auth !== token) return;
    state = next; connected = true;
    if (!state.sessions.some(s => s.id === selected)) selected = state.sessions[0]?.id || '';
    const id = selected;
    const nextDetail = id ? await api(`/api/sessions/${encodeURIComponent(id)}`) : null;
    if (auth !== token) return;
    if (selected === id) detail = nextDetail;
    $('login').hidden = true; $('console').hidden = false; $('login-error').textContent = '';
    $('page-error').hidden = true; render();
  } catch (err) {
    if (auth !== token) return;
    connected = false;
    if (err.status === 401) { token = ''; sessionStorage.removeItem('gateway-token'); showLogin(); $('login-error').textContent = err.message; }
    else { showError('Gateway 连接中断，将自动恢复。'+err.message); $('login-error').textContent = err.message; renderComposer(); }
  } finally { loading = false; }
}
function switchSession(id) { selected = id; detail = null; lastTimeline = ''; sessionStorage.setItem('gateway-session', id); render(); refresh(); }
function render() {
  const list = $('session-list'); list.replaceChildren();
  state.sessions.forEach(session => {
    const item = node('button', undefined, 'session-item'+(session.id === selected ? ' selected' : ''));
    item.append(node('div', session.title, 'session-title'));
    const meta = node('div', undefined, 'session-meta'); meta.append(node('i', undefined, `dot ${session.active_task_id ? 'running' : session.mode}`), node('span', session.executor_name));
    item.append(meta); item.onclick = () => switchSession(session.id); list.append(item);
  });
  if (!state.sessions.length) list.append(node('p', '还没有会话', 'subtle'));
  $('pool-count').textContent = `${state.executors.filter(e => e.online).length} 在线 / ${state.executors.length} 台机器`;
  const session = detail?.session || state.sessions.find(s => s.id === selected);
  $('chat-title').textContent = session?.title || '新的一项工作';
  $('binding').textContent = session ? `${session.executor_name} · ${session.workdir}` : '先选择执行端，再开始会话';
  $('mode').textContent = session ? modeNames[session.mode] : '未选择执行端';
  $('mode').className = 'badge '+(session?.mode || '');
  $('model-banner').hidden = state.model_ready;
  $('offline-banner').hidden = session?.mode !== 'chat_only';
  updateExecutorOptions(); renderTimeline(); renderComposer();
}
function renderComposer() {
  const session = detail?.session || state.sessions.find(s => s.id === selected);
  const running = Boolean(session?.active_task_id);
  $('message').disabled = !session || !state.model_ready || !connected;
  $('send-message').disabled = !session || !state.model_ready || !connected || running || submitting || session.mode === 'busy' || !$('message').value.trim();
  $('send-message').hidden = running;
  $('stop-turn').hidden = !running;
  $('composer-note').textContent = !session ? '每个会话固定绑定一台 Executor' : session.mode === 'chat_only' ? '仅聊天 · 机器工具已禁用' : session.mode === 'busy' ? '执行端正在处理其他会话' : '完全访问 · 工具直接执行';
}
function renderText(parent, text) {
  const parts = String(text).split(/```[^\n]*\n([\s\S]*?)```/g);
  parts.forEach((part, index) => { if (part) parent.append(node(index % 2 ? 'pre' : 'p', part)); });
}
function renderTimeline() {
  const key = selected + JSON.stringify(detail);
  if (key === lastTimeline) return;
  lastTimeline = key;
  const timeline = $('timeline'), nearBottom = timeline.scrollHeight - timeline.scrollTop - timeline.clientHeight < 100;
  const expanded = new Set([...timeline.querySelectorAll('details[open]')].map(d => d.dataset.id));
  timeline.replaceChildren();
  if (!detail?.turns.length) {
    const welcome = node('div', undefined, 'welcome'); welcome.append(node('span', '⌘', 'welcome-icon'), node('h2', selected ? '从一个具体任务开始。' : '今天想完成什么？'), node('p', selected ? '读一个文件、运行脚本，或者继续讨论想法。' : '选择一台在线机器，开始一段会话。'));
    if (!selected) { const button = node('button', '新建会话', 'primary'); button.onclick = openNew; welcome.append(button); }
    timeline.append(welcome); return;
  }
  detail.turns.forEach(turn => {
    const section = node('article', undefined, 'turn'); section.append(node('div', turn.message, 'message user-message'));
    const tools = new Map(); let answer = '';
    (turn.events || []).forEach(event => {
      if (event.type === 'assistant') { const message = node('div', undefined, 'message assistant-message'); renderText(message, event.text); section.append(message); answer = event.text; }
      if (event.type === 'tool_start' || event.type === 'tool_result') {
        let block = tools.get(event.call_id);
        if (!block) {
          const box = node('details', undefined, 'tool'); box.dataset.id = `${turn.id}:${event.call_id}`; box.open = expanded.has(box.dataset.id);
          const summary = node('summary'); summary.append(node('strong', event.name || '工具'));
          const status = node('span', '执行中', 'tool-state'); summary.append(status);
          box.append(summary); const content = node('pre'); box.append(content); section.append(box);
          block = {box, status, content, arguments: null}; tools.set(event.call_id, block);
        }
        if (event.type === 'tool_start') block.arguments = event.arguments;
        else { block.output = event.output; block.status.textContent = event.blocked ? '已阻止 · 仅聊天' : '已返回'; }
        const output = typeof block.output === 'string' ? block.output : block.output === undefined ? '' : JSON.stringify(block.output, null, 2);
        block.content.textContent = (block.arguments ? JSON.stringify(block.arguments, null, 2)+'\n\n' : '') + output;
      }
      if (event.type === 'executor_offline') section.append(node('p', 'Executor 已离线，本轮切换为仅聊天。未确认的操作不会重放。', 'turn-note'));
    });
    if (turn.results?.answer && turn.results.answer !== answer) { const message = node('div', undefined, 'message assistant-message'); renderText(message, turn.results.answer); section.append(message); }
    if (turn.events_truncated) section.append(node('p', '部分工具记录已截断。', 'turn-note'));
    if (turn.status === 'running') {
      const last = turn.events?.at(-1);
      section.append(node('div', last?.type === 'tool_start' ? `正在执行 ${last.name}…` : turn.chat_only ? '正在回复 · 仅聊天…' : '正在处理…', 'activity'));
    } else {
      tools.forEach(block => { if (block.output === undefined) block.status.textContent = '未收到结果'; });
      if (turn.error) section.append(node('p', turn.error, turn.status === 'canceled' ? 'turn-note' : 'turn-error'));
    }
    timeline.append(section);
  });
  if (nearBottom) timeline.scrollTop = timeline.scrollHeight;
}
function updateExecutorOptions() {
  const select = $('executor-select'), previous = select.value;
  const available = state.executors.filter(e => e.online && !e.task_id);
  const key = JSON.stringify(available);
  if (select.dataset.options !== key) {
    select.replaceChildren(new Option(available.length ? '选择在线执行端' : '没有在线空闲的 Executor', ''));
    available.forEach(e => select.append(new Option(`${e.name} · ${e.os}/${e.arch}`, e.id)));
    if (available.some(e => e.id === previous)) select.value = previous;
    select.dataset.options = key;
  }
  $('create-session').disabled = !connected || !select.value;
}
function openNew() { $('session-error').textContent = ''; updateExecutorOptions(); $('new-dialog').showModal(); }
$('new-chat').onclick = openNew; $('welcome-new').onclick = openNew;
$('close-new').onclick = () => $('new-dialog').close(); $('executor-select').onchange = updateExecutorOptions;
$('session-form').onsubmit = async event => {
  event.preventDefault(); $('create-session').disabled = true; $('session-error').textContent = '';
  try { const session = await api('/api/sessions', 'POST', {executor_id: $('executor-select').value, workdir: $('workdir').value.trim()}); $('new-dialog').close(); switchSession(session.id); }
  catch (err) { $('session-error').textContent = err.message; }
  finally { updateExecutorOptions(); }
};
$('message').oninput = () => { $('message').style.height = 'auto'; $('message').style.height = Math.min(200, $('message').scrollHeight)+'px'; renderComposer(); };
$('message').onkeydown = event => { if (event.key === 'Enter' && !event.shiftKey && !event.isComposing) { event.preventDefault(); if (!$('send-message').disabled) $('message-form').requestSubmit(); } };
$('message-form').onsubmit = async event => {
  event.preventDefault(); if (submitting || $('send-message').disabled) return;
  const session = selected, text = $('message').value;
  submitting = true; renderComposer();
  try { await api(`/api/sessions/${encodeURIComponent(session)}/messages`, 'POST', {message:text}); if (session === selected && $('message').value === text) { $('message').value = ''; $('message').style.height = 'auto'; } await refresh(); }
  catch (err) { showError(err.message); }
  finally { submitting = false; renderComposer(); }
};
$('stop-turn').onclick = async () => {
  const id = detail?.session.active_task_id; if (!id) return;
  $('stop-turn').disabled = true;
  try { await api(`/api/tasks/${encodeURIComponent(id)}/cancel`, 'POST', {}); await refresh(); }
  catch (err) { showError(err.message); }
  finally { $('stop-turn').disabled = false; }
};
$('login-form').onsubmit = async event => { event.preventDefault(); token = $('admin-token').value.trim(); sessionStorage.setItem('gateway-token', token); await refresh(); if (connected) $('admin-token').value = ''; };
$('logout').onclick = () => { token = ''; detail = null; state.sessions = []; connected = false; sessionStorage.removeItem('gateway-token'); showLogin(); };
function checkBuild() { const ready = state.downloads?.[$('executor-platform').value]; $('install-error').textContent = ready ? '' : '该平台安装包尚未准备好。'; $('generate-install').disabled = !ready; $('install-output').hidden = true; }
$('executor-platform').onchange = checkBuild;
$('add-executor').onclick = () => { $('gateway-origin').value = location.origin; checkBuild(); $('install-dialog').showModal(); };
$('close-install').onclick = () => $('install-dialog').close();
const quote = value => "'"+value.replaceAll("'", "'\\''")+"'";
$('generate-install').onclick = async () => {
  $('install-error').textContent = ''; $('generate-install').disabled = true;
  try {
    const raw = $('gateway-origin').value.replace(/\/$/, ''), url = new URL(raw);
    if (!['http:', 'https:'].includes(url.protocol) || url.origin !== raw) throw new Error('请输入不包含路径的 Gateway 地址。');
    const name = $('executor-name').value.trim(); if (!name) throw new Error('请填写机器名称。');
    const result = await api('/api/enrollments', 'POST', {}), platform = $('executor-platform').value.replace('/', '-');
    $('install-command').textContent = `curl -fsSL ${quote(raw+'/install.sh')} | sh -s -- --gateway ${quote(raw)} --token ${quote(result.token)} --name ${quote(name)} --platform ${quote(platform)}`;
    $('expiry').textContent = `一次有效 · ${new Date(result.expires_at).toLocaleTimeString('zh-CN')} 前使用`;
    $('install-output').hidden = false; $('copy-command').textContent = '复制';
  } catch (err) { $('install-error').textContent = err.message; }
  finally { $('generate-install').disabled = false; }
};
$('copy-command').onclick = async () => { try { await navigator.clipboard.writeText($('install-command').textContent); $('copy-command').textContent = '已复制'; } catch { $('install-error').textContent = '请手动复制上方命令。'; } };
function showModelKeyState(config) {
  $('model-api-key').value = '';
  $('model-api-key').required = !config.api_key_set;
  $('model-api-key').placeholder = config.api_key_set ? '已保存 · 留空保留原密钥' : '填写 API 密钥';
  $('model-key-hint').textContent = config.api_key_set ? '密钥已保存在服务端；填写新值可替换，留空保持原值。' : '密钥只保存到服务端，不会在页面回显。';
}
async function openModelSettings() {
  $('model-error').textContent = ''; $('model-saved').textContent = '';
  $('model-api-key').value = ''; $('save-model').disabled = true;
  $('model-dialog').showModal();
  try {
    const config = await api('/api/model-config');
    $('model-base-url').value = config.base_url;
    $('model-name').value = config.model;
    showModelKeyState(config); $('save-model').disabled = false;
  } catch (err) { $('model-error').textContent = err.message; }
}
$('model-settings').onclick = openModelSettings; $('configure-model').onclick = openModelSettings;
$('close-model').onclick = () => $('model-dialog').close();
$('model-dialog').onclose = () => { $('model-api-key').value = ''; };
$('model-form').onsubmit = async event => {
  event.preventDefault(); if ($('save-model').disabled) return;
  $('save-model').disabled = true; $('model-error').textContent = ''; $('model-saved').textContent = '';
  try {
    const config = await api('/api/model-config', 'PUT', {base_url:$('model-base-url').value.trim(), model:$('model-name').value.trim(), api_key:$('model-api-key').value.trim()});
    showModelKeyState(config);
    $('model-saved').textContent = '已保存，下一轮生效。实际连接结果会在发送消息后显示。';
    await refresh();
  } catch (err) { $('model-error').textContent = err.message; }
  finally { $('save-model').disabled = false; }
};
showLogin(); refresh(); setInterval(() => { if (!document.hidden) refresh(); }, 1000);
