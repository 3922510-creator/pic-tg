'use strict';

const $ = (s, r = document) => r.querySelector(s);
const $$ = (s, r = document) => [...r.querySelectorAll(s)];
const esc = s => String(s).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

function fmtSize(kb) {
  return kb < 1 ? Math.round(kb * 1024) + ' B' : kb >= 1024 ? (kb / 1024).toFixed(2) + ' MB' : Math.round(kb) + ' KB';
}

let toastTimer = null;
function toast(msg) {
  const t = $('#toast');
  t.textContent = msg;
  t.classList.add('show');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.classList.remove('show'), 2200);
}

async function copyText(text, msg) {
  try {
    await navigator.clipboard.writeText(text);
    toast(msg || '已复制');
    return true;
  } catch (e) {
    toast('复制失败，请手动长按复制');
    return false;
  }
}

// Reuse unchanged cards (and their decoded images); only insert/remove changed
// records. Event handlers are delegated once to their stable parent containers.
function syncKeyedCards(container, records) {
  const existing = new Map([...container.children].map(node => [node.dataset.renderKey, node]));
  const retained = new Set();
  for (let index = 0; index < records.length; index++) {
    const { key, html } = records[index];
    let node = existing.get(key);
    if (!node || node.renderHTML !== html) {
      const template = document.createElement('template');
      template.innerHTML = html;
      const replacement = template.content.firstElementChild;
      replacement.dataset.renderKey = key;
      replacement.renderHTML = html;
      if (node) node.replaceWith(replacement);
      node = replacement;
    }
    if (container.children[index] !== node) container.insertBefore(node, container.children[index] || null);
    retained.add(node);
  }
  for (const node of [...container.children]) {
    if (!retained.has(node)) node.remove();
  }
}

/* ================= 凭据管理 ================= */
const TOKEN_KEY = 'pic_tg_auth_token';
const LEGACY_TOKEN_KEY = 'picbed_auth_token';
let serverHasAuth = true;

function getToken() {
  return localStorage.getItem(TOKEN_KEY) || localStorage.getItem(LEGACY_TOKEN_KEY) || '';
}

function setToken(t) {
  if (t) {
    localStorage.setItem(TOKEN_KEY, t);
    localStorage.removeItem(LEGACY_TOKEN_KEY);
  } else {
    localStorage.removeItem(TOKEN_KEY);
    localStorage.removeItem(LEGACY_TOKEN_KEY);
    authGeneration++;
    setAuthLocked(true);
    clearPrivateUI();
    updateTokenUI();
  }
}

function authHeaders(extra = {}, token = getToken()) {
  const headers = { ...extra };
  if (token) headers.Authorization = 'Bearer ' + token;
  return headers;
}

function setAuthLocked(locked) {
  document.body.classList.toggle('auth-locked', locked);
  for (const el of document.body.children) {
    if (!['authOverlay', 'toast'].includes(el.id) && el.tagName !== 'SCRIPT') el.inert = locked;
  }
  if (locked) {
    for (const id of ['pwdOverlay', 'confirmOverlay']) $('#' + id)?.classList.remove('show');
    closeRecentZoom();
  }
}

async function authFetch(url, options = {}) {
  const token = getToken();
  const res = await fetch(url, options);
  if (res.status === 401 && getToken() === token) {
    setToken('');
    promptAuthModal('登录已失效，请重新输入口令');
  }
  return res;
}

/* ================= 浏览器质量压缩；服务端仍仅做无损优化 ================= */
const uploadMaxBytes = 20 * 1024 * 1024;
const compressionInputMaxBytes = 50 * 1024 * 1024;

function canvasOutputFormat(file, quality) {
  return quality === 100 ? 'png' : /^image\/jpe?g$/i.test(file.type) ? 'jpg' : 'webp';
}

async function encodeCanvas(canvas, name, ext, quality) {
  const type = ext === 'jpg' ? 'image/jpeg' : 'image/' + ext;
  const blob = await new Promise(resolve => canvas.toBlob(resolve, type, quality < 100 ? quality / 100 : undefined));
  if (!blob || blob.type !== type) {
    throw new Error(quality < 100 ? '浏览器无法压缩这张图，请换浏览器或选原画质' : '无法生成图片，请尝试较小的图片');
  }
  return new File([blob], name + '.' + ext, { type });
}

async function prepareImage(file, quality = 100) {
  const compress = quality < 100 && editableRaster(file) && await isStaticRaster(file);
  if (!compress && (/^image\/(png|gif|jpe?g|webp)$/i.test(file.type) || /\.(png|gif|jpe?g|webp)$/i.test(file.name || ''))) {
    return file;
  }
  let bitmap;
  if (window.createImageBitmap) {
    try { bitmap = await createImageBitmap(file); } catch (e) {}
  }
  if (!bitmap) {
    bitmap = await new Promise((resolve, reject) => {
      const img = new Image();
      const url = URL.createObjectURL(file);
      img.onload = () => { URL.revokeObjectURL(url); resolve(img); };
      img.onerror = () => { URL.revokeObjectURL(url); reject(new Error('浏览器无法读取这张图片，请先转换为 PNG 或 JPG')); };
      img.src = url;
    });
  }
  const canvas = document.createElement('canvas');
  try {
    const w = bitmap.width;
    const h = bitmap.height;
    if (!w || !h || w * h > 50_000_000) throw new Error('图片超过 5000 万像素或无法读取');
    canvas.width = w;
    canvas.height = h;
    const ctx = canvas.getContext('2d');
    if (!ctx) throw new Error('无法创建图片画布，请尝试较小的图片');
    ctx.drawImage(bitmap, 0, 0, w, h);
    const outputQuality = compress ? quality : 100;
    const output = await encodeCanvas(canvas, file.name.replace(/\.[^.]+$/, ''), canvasOutputFormat(file, outputQuality), outputQuality);
    return compress && output.size >= file.size ? file : output;
  } finally {
    canvas.width = 1;
    canvas.height = 1;
    if (bitmap.close) bitmap.close();
  }
}

/* ================= PicAPI 真实适配层 ================= */
const PicAPI = {
  async fetchStatus(authOnly = false) {
    try {
      const res = await authFetch('/api/status' + (authOnly ? '?auth_only=1' : ''), {
        headers: authHeaders(),
        cache: 'no-store'
      });
      if (!res.ok) throw new Error('服务暂不可用');
      const json = await res.json();
      const d = json.data || {};
      serverHasAuth = !!d.has_auth;
      return d;
    } catch (e) {
      return { has_auth: serverHasAuth, unavailable: true };
    }
  },

  async list() {
    if (serverHasAuth && !getToken()) return [];
    const token = getToken();
    const headers = authHeaders({}, token);
    const records = [];
    for (let offset = 0; ;) {
      const res = await authFetch('/api/objects?limit=1000&offset=' + offset, { headers });
      const json = await res.json();
      if (!res.ok || json.code !== 200) {
        if (res.status === 401 && getToken() === token) {
          setToken('');
          updateTokenUI();
          renderGallery();
          promptAuthModal('需要重新登录以访问图库');
        }
        throw new Error(json.msg || '图库加载失败');
      }
      const page = json.data?.list || [];
      records.push(...page);
      offset += page.length;
      if (!page.length || offset >= json.data.total) break;
    }
    return records.map((it, idx) => ({
      id: it.key,
      key: it.key,
      name: it.key.split('/').pop(),
      sizeKB: it.size / 1024,
      origKB: (it.orig_size || it.size) / 1024,
      ts: it.created_at ? Date.parse(it.created_at.replace(' ', 'T') + 'Z') : (Date.now() - idx * 60000),
      width: it.width,
      height: it.height,
      backend: it.backend || 'telegram',
      thumbUrl: it.thumb_url,
      proxyUrl: it.proxy_url,
      directUrl: it.direct_url,
      url: it.proxy_url || it.direct_url
    }));
  },

  async upload(file, onProgress, quality = 100) {
    if (serverHasAuth && !getToken()) {
      promptAuthModal('请先输入口令登录图床');
      throw new Error('未授权，请先登录');
    }
    const mayCompress = quality < 100 && editableRaster(file);
    if (file.size > compressionInputMaxBytes) throw new Error('原文件超过 50 MB，请先缩小图片');
    if (file.size > uploadMaxBytes && !mayCompress) {
      throw new Error('超过 TG 20 MB 上限；静态 JPG、PNG、WebP 可调低压缩质量再试');
    }

    onProgress(10, mayCompress ? '正在准备压缩' : '正在准备图片');

    const origSize = file.size;
    const fileToUpload = await prepareImage(file, quality);
    if (fileToUpload.size > uploadMaxBytes) {
      throw new Error(fileToUpload === file
        ? '图片仍超过 TG 20 MB 上限，请降低质量或先缩小图片'
        : '处理后的图片超过 TG 20 MB 上限，请降低质量或先缩小图片');
    }

    onProgress(25, '正在发送图片');

    const token = getToken();
    const formData = new FormData();
    formData.append('file', fileToUpload);
    formData.append('orig_size', String(origSize));

    return new Promise((resolve, reject) => {
      const xhr = new XMLHttpRequest();
      xhr.open('POST', '/api/upload', true);
      xhr.timeout = 270000; // Longer than the server's bounded 240-second handler.
      if (token) xhr.setRequestHeader('Authorization', 'Bearer ' + token);

      xhr.upload.onprogress = e => {
        if (e.lengthComputable) {
          const p = 25 + Math.min(73, Math.floor((e.loaded / e.total) * 73));
          onProgress(p, p >= 98 ? '正在存入图床，请稍候' : '正在发送图片');
        }
      };
      xhr.upload.onload = () => onProgress(98, '正在存入图床，请稍候');

      xhr.onload = () => {
        try {
          if (xhr.status === 401) {
            if (getToken() === token) {
              setToken('');
              updateTokenUI();
              refreshStatus();
              renderGallery();
              promptAuthModal('图床口令无效或已过期，请重新输入');
            }
            reject(new Error('未授权，请先登录'));
            return;
          }
          const res = JSON.parse(xhr.responseText);
          if (xhr.status === 200 && res.code === 200) {
            onProgress(100);
            const d = res.data;
            resolve({
              url: d.proxy_url || d.url,
              directUrl: d.direct_url,
              proxyUrl: d.proxy_url,
              thumbUrl: d.thumb_url,
              key: d.key,
              backend: d.backend || 'telegram',
              sizeKB: d.size / 1024,
              origKB: origSize / 1024
            });
          } else {
            reject(new Error(res.msg || '上传失败'));
          }
        } catch (err) {
          reject(new Error('服务器返回异常（HTTP ' + xhr.status + '），请先刷新图库确认是否成功，再重试'));
        }
      };

      xhr.onerror = () => reject(new Error('网络连接中断，请先刷新图库确认是否成功，再重试'));
      xhr.ontimeout = () => reject(new Error('图片存储超时，请先刷新图库确认是否成功，再重试'));
      xhr.onabort = () => reject(new Error('上传已取消，可以重试'));
      xhr.send(formData);
    });
  },

  async batchRemove(keys) {
    const res = await authFetch('/api/objects', {
      method: 'DELETE',
      headers: authHeaders({ 'Content-Type': 'application/json' }),
      body: JSON.stringify({ keys })
    });
    const json = await res.json();
    if (!res.ok || json.code !== 200) throw new Error(json.msg || '删除失败');
    return json.data;
  },

  async changePassword(oldPwd, newPwd) {
    const res = await authFetch('/api/admin/password', {
      method: 'POST',
      headers: authHeaders({ 'Content-Type': 'application/json' }),
      body: JSON.stringify({ old_password: oldPwd, new_password: newPwd })
    });
    const json = await res.json();
    if (json.code === 200) {
      setToken(newPwd);
      return true;
    }
    throw new Error(json.msg || '口令修改失败');
  }
};

/* ================= 状态 ================= */
let items = [];
const FORMATS = ['url', 'md', 'bb', 'html'];
const SORT_OPTIONS = { new: '最新优先', old: '最早优先', big: '体积大到小', small: '体积小到大' };
const savedFmt = (() => {
  try {
    const v = localStorage.getItem('pic-format');
    return FORMATS.includes(v) ? v : 'url';
  } catch (e) {
    return 'url';
  }
})();
const state = {
  page: 'upload',
  queue: [],
  folder: 'all',
  sort: 'new',
  selectMode: false,
  selected: new Set(),
  fmt: savedFmt
};

function setDefaultFormat(fmt) {
  if (!FORMATS.includes(fmt)) fmt = 'url';
  state.fmt = fmt;
  inspectorFormat = fmt;
  try { localStorage.setItem('pic-format', fmt); } catch (e) {}
  $$('#defaultFormatSeg button').forEach(x => x.classList.toggle('on', x.dataset.format === fmt));
  $$('#fmtTabs button').forEach(x => x.classList.toggle('on', x.dataset.f === fmt));
  $$('#inspFmtSeg button').forEach(x => x.classList.toggle('on', x.dataset.fmt === fmt));
}

function clearPrivateUI() {
  closeRedaction(false);
  resetTelegramPrivate();
  clearRecentMemory();
  closeInspector();
  setSortMenuOpen(false);
  pageInitialized = false;
  galleryRequest++;
  items = [];
  state.queue.forEach(q => URL.revokeObjectURL(q.preview));
  state.queue = [];
  apiKeyValue = '';
  apiKeyRequest++;
  tokenShown = false;
  state.selected.clear();
  state.selectMode = false;
  closeRecentZoom();
  $('#selectModeBtn').textContent = '选择';
  $('#batchbar').classList.remove('show');
  $('#queue').replaceChildren();
  $('#queueActions').hidden = true;
  $('#queueSummary').textContent = '';
  $('#grid').replaceChildren();
  state.folder = 'all';
  $('#recentCard').hidden = true;
  $('#recentEmpty').hidden = false;
  $('#uploadBtn').classList.add('is-idle');
}

/* ================= 页面切换 ================= */
const PAGES = ['upload', 'gallery', 'api', 'settings'];
let pageInitialized = false;
$$('[data-page]').forEach(b => b.addEventListener('click', () => showPage(b.dataset.page)));
window.addEventListener('hashchange', () => showPage(location.hash.slice(1)));

/* ================= 主题 ================= */
const mq = matchMedia('(prefers-color-scheme: dark)');
const themeSeg = $('#themeSeg');

function applyThemeMode(mode) {
  const t = mode === 'system' ? (mq.matches ? 'dark' : 'light') : mode;
  document.documentElement.dataset.theme = t;
  document.documentElement.dataset.themeMode = mode;
  try { localStorage.setItem('pic-theme', mode); } catch (e) {}
  $('#metaThemeColor').content = t === 'dark' ? '#0b0b0d' : '#ffffff';
  $$('#themeSeg button').forEach(b => b.classList.toggle('on', b.dataset.m === mode));
}

themeSeg.addEventListener('click', e => {
  const b = e.target.closest('button');
  if (b) applyThemeMode(b.dataset.m);
});
$('#themeToggle').addEventListener('click', () => applyThemeMode(document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark'));
if (mq.addEventListener) {
  mq.addEventListener('change', () => {
    if (document.documentElement.dataset.themeMode === 'system') applyThemeMode('system');
  });
}
applyThemeMode(document.documentElement.dataset.themeMode || 'system');

/* ================= 通用确认弹窗 ================= */
let confirmResolve = null;

function confirmDialog(title, body, okText) {
  $('#confirmTitle').textContent = title;
  $('#confirmBody').textContent = body || '';
  $('#confirmOk').textContent = okText || '确认';
  $('#confirmOverlay').classList.add('show');
  return new Promise(res => { confirmResolve = res; });
}

function closeConfirm(v) {
  $('#confirmOverlay').classList.remove('show');
  if (confirmResolve) {
    confirmResolve(v);
    confirmResolve = null;
  }
}

$('#confirmOk').addEventListener('click', () => closeConfirm(true));
$('#confirmCancel').addEventListener('click', () => closeConfirm(false));
$('#confirmOverlay').addEventListener('click', e => {
  if (e.target.id === 'confirmOverlay') closeConfirm(false);
});

/* ================= 全屏口令登录 ================= */
let authCallback = null;
let authBusy = false;
let authGeneration = 0;

function promptAuthModal(msg, cb) {
  authCallback = cb;
  setAuthLocked(true);
  $('#authMsg').textContent = msg || '输入访问口令以进入图床';
  $('#authInput').value = '';
  setTimeout(() => $('#authInput').focus(), 0);
}

function closeAuthModal() {
  // Escape/backdrop calls cannot unlock a login gate.
}

$('#authForm').addEventListener('submit', async e => {
  e.preventDefault();
  if (authBusy) return;
  const val = $('#authInput').value.trim();
  if (!val) {
    $('#authMsg').textContent = '请输入访问口令';
    return;
  }
  authBusy = true;
  const generation = ++authGeneration;
  $('#authSubmitBtn').disabled = true;
  $('#authSubmitBtn').textContent = '正在验证…';
  try {
    const r = await fetch('/api/auth/verify', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ token: val })
    });
    const res = await r.json();
    if (generation !== authGeneration) return;
    if (r.ok && res.code === 200) {
      setToken(val);
      $('#authInput').value = '';
      const cb = authCallback;
      authCallback = null;
      setAuthLocked(false);
      enterPrivateUI();
      if (cb) cb();
    } else {
      $('#authMsg').textContent = res.msg || '口令错误，请重试';
      $('#authInput').select();
    }
  } catch (err) {
    if (generation === authGeneration) $('#authMsg').textContent = '网络连接失败，请重试';
  } finally {
    authBusy = false;
    $('#authSubmitBtn').disabled = false;
    $('#authSubmitBtn').textContent = '登录图床';
  }
});

/* ================= 修改密码弹窗 ================= */
function openPwdModal() {
  if (serverHasAuth && !getToken()) {
    toast('当前未登录');
    promptAuthModal();
    return;
  }
  $('#oldPwdInput').value = '';
  $('#newPwdInput').value = '';
  $('#pwdOverlay').classList.add('show');
  setTimeout(() => $('#oldPwdInput').focus(), 100);
}

function closePwdModal() {
  $('#pwdOverlay').classList.remove('show');
}

$('#btnChangePwd').addEventListener('click', openPwdModal);
$('#pwdCancelBtn').addEventListener('click', closePwdModal);
$('#pwdOverlay').addEventListener('click', e => {
  if (e.target.id === 'pwdOverlay') closePwdModal();
});
$('#pwdSubmitBtn').addEventListener('click', async () => {
  const oldPwd = $('#oldPwdInput').value.trim();
  const newPwd = $('#newPwdInput').value.trim();
  if ((serverHasAuth && !oldPwd) || !newPwd) {
    toast('请完整填写密码');
    return;
  }
  if (newPwd.length < 6) {
    toast('新密码长度不能少于 6 位');
    return;
  }
  try {
    await PicAPI.changePassword(oldPwd, newPwd);
    closePwdModal();
    updateTokenUI();
    toast('密码修改成功，新口令已生效');
  } catch (err) {
    toast('修改失败: ' + err.message);
  }
});

/* ================= 设置页账号与状态刷新 ================= */
async function refreshStatus() {
  const requestToken = getToken();
  const data = await PicAPI.fetchStatus();
  if (requestToken !== getToken()) return;
  if (!data.unavailable && data.has_auth && data.authenticated === false) {
    setToken('');
    promptAuthModal('登录已失效，请重新输入口令');
    return;
  }
  if (data.unavailable) {
    $('#storageStatusText').textContent = '状态获取失败，请稍后重试';
    return;
  }
  const token = getToken();
  if (data.has_auth) {
    $('#loginStatus').textContent = token ? '已登录 · 认证开启' : '未登录 (点击输入口令)';
    $('#loginStatus').style.color = token ? 'var(--ok)' : 'var(--danger)';
    $('#loginStatus').style.cursor = token ? 'default' : 'pointer';
    $('#loginStatus').onclick = token ? null : () => promptAuthModal();
  } else {
    $('#loginStatus').textContent = '公开访问 (免密)';
    $('#loginStatus').style.color = 'var(--muted)';
  }

  if (data.storage) {
    const label = 'Telegram 私密频道';
    $('#storageStatusText').textContent = label + (data.storage.ok ? ' · 已连接' : ' · 连接失败');
    $('#storageStatusText').style.color = data.storage.ok ? 'var(--ok)' : 'var(--danger)';
    $('#storageStatusText').title = data.storage.detail || '';
  }
}

$('#btnLogout').addEventListener('click', async () => {
  if (!getToken()) {
    toast('当前尚未登录');
    if (serverHasAuth) promptAuthModal();
    return;
  }
  const ok = await confirmDialog('退出图床登录', '退出后本设备需要重新输入口令才能管理图库，公开图片链接不受影响。确定退出？', '退出');
  if (!ok) return;
  setToken('');
  renderGallery();
  updateTokenUI();
  refreshStatus();
  toast('已退出登录');
  promptAuthModal('已退出登录，请输入口令重新登录');
});

/* ================= Telegram 接入设置 ================= */
/* Secrets are never returned by the server or stored in browser storage. */
let tgEpoch = 0;
let tgSettings = null;
let tgBusy = false;

$('#tgManage').addEventListener('click', () => {
  if (serverHasAuth && !getToken()) {
    promptAuthModal();
    return;
  }
  $('#tgDialog').showModal();
  loadTelegramSettings();
});
$('#tgClose').addEventListener('click', () => $('#tgDialog').close());
$('#tgDialog').addEventListener('close', () => {
  resetTelegramPrivate();
  if (!document.body.classList.contains('auth-locked')) $('#tgManage').focus();
});
$('#tgDialog').addEventListener('click', e => {
  if (e.target !== e.currentTarget) return;
  const r = e.currentTarget.getBoundingClientRect();
  if (e.clientX < r.left || e.clientX > r.right || e.clientY < r.top || e.clientY > r.bottom) {
    e.currentTarget.close();
  }
});

function resetTelegramPrivate() {
  if ($('#tgDialog').open) $('#tgDialog').close();
  tgEpoch++;
  tgSettings = null;
  tgBusy = false;
  $('#tgToken').value = '';
  $('#tgChatID').value = '';
  $('#tgConnection').replaceChildren(new Option('新增接入', ''));
  $('#tgChannels').replaceChildren();
  $('#tgChannelsGroup').hidden = true;
  $('#tgFeedback').textContent = '请登录后设置';
  for (const id of ['tgVerify', 'tgDiscover', 'tgSave']) $('#' + id).disabled = false;
}

async function tgRequest(action, body) {
  const res = await authFetch('/api/admin/telegram' + (action ? '/' + action : ''), {
    method: action ? 'POST' : 'GET',
    headers: authHeaders({ 'Content-Type': 'application/json' }),
    ...(action ? { body: JSON.stringify(body) } : {})
  });
  const json = await res.json();
  if (!res.ok || json.code !== 200) throw new Error(json.msg || '请求失败');
  return json.data;
}

function displayTelegramSettings(data) {
  tgSettings = data;
  const select = $('#tgConnection');
  select.replaceChildren(new Option('新增接入', ''));
  for (const p of data.connections) {
    select.add(new Option((p.id === data.active_id ? '当前 · ' : '') + (p.chat_title || p.chat_id) + ' · @' + p.bot_username, p.id));
  }
  select.value = data.active_id;
  selectTelegramConnection();
}

function selectTelegramConnection() {
  tgEpoch++;
  $('#tgToken').value = '';
  $('#tgChannelsGroup').hidden = true;
  $('#tgChannels').replaceChildren();
  const p = tgSettings?.connections.find(item => item.id === $('#tgConnection').value);
  $('#tgChatID').value = p ? String(p.chat_id) : '';
  $('#tgToken').placeholder = p ? '留空沿用已保存 Token' : '粘贴 Bot Token';
  $('#tgFeedback').textContent = p ? '已保存 · @' + p.bot_username + ' · ' + p.chat_title : '填入 Token，验证机器人后选择频道';
}

async function loadTelegramSettings() {
  if (!getToken() && serverHasAuth) return;
  const epoch = ++tgEpoch;
  const token = getToken();
  try {
    const data = await tgRequest('');
    if (epoch !== tgEpoch || token !== getToken()) return;
    displayTelegramSettings(data);
  } catch (e) {
    if (epoch === tgEpoch) $('#tgFeedback').textContent = e.message;
  }
}

$('#tgConnection').addEventListener('change', selectTelegramConnection);
for (const id of ['tgToken', 'tgChatID']) {
  $('#' + id).addEventListener('input', () => {
    tgEpoch++;
    $('#tgFeedback').textContent = '未保存';
  });
}
$('#tgChannels').addEventListener('change', () => {
  tgEpoch++;
  $('#tgChatID').value = $('#tgChannels').value;
  $('#tgFeedback').textContent = '频道已选择，保存后生效';
});

async function telegramAction(action) {
  if (tgBusy) return;
  if (!tgSettings) {
    await loadTelegramSettings();
    if (!tgSettings) return;
  }
  const epoch = ++tgEpoch;
  const token = getToken();
  tgBusy = true;
  for (const id of ['tgVerify', 'tgDiscover', 'tgSave']) $('#' + id).disabled = true;
  $('#tgFeedback').textContent = '正在' + ({ verify: '验证机器人', discover: '查找频道', save: '校验权限并保存' })[action] + '…';
  try {
    const value = $('#tgChatID').value.trim();
    if (action === 'save' && !/^-[1-9][0-9]*$/.test(value)) throw new Error('请填写负数频道 ID');
    const data = await tgRequest(action, {
      token: $('#tgToken').value.trim(),
      connection_id: $('#tgConnection').value,
      chat_id: value ? Number(value) : 0,
      revision: tgSettings.revision
    });
    if (epoch !== tgEpoch || token !== getToken()) return;
    if (action === 'verify') {
      $('#tgFeedback').textContent = '验证成功 · @' + data.username + ' · ID ' + data.id;
    } else if (action === 'discover') {
      const select = $('#tgChannels');
      select.replaceChildren(new Option('请选择频道', ''));
      for (const c of data.channels) select.add(new Option(c.title + ' · ' + c.id, String(c.id)));
      $('#tgChannelsGroup').hidden = !data.channels.length;
      $('#tgFeedback').textContent = data.channels.length
        ? '找到 ' + data.channels.length + ' 个频道，请选择后保存'
        : '未找到频道。请先添加机器人为频道管理员，在频道发新消息后重试，或手填 ID。';
    } else {
      displayTelegramSettings(data);
      $('#tgFeedback').textContent = '已保存并立即生效，旧图仍可访问';
      refreshStatus();
    }
  } catch (e) {
    if (epoch === tgEpoch) $('#tgFeedback').textContent = e.message;
  } finally {
    tgBusy = false;
    for (const id of ['tgVerify', 'tgDiscover', 'tgSave']) $('#' + id).disabled = false;
  }
}

for (const [id, action] of [['tgVerify', 'verify'], ['tgDiscover', 'discover'], ['tgSave', 'save']]) {
  $('#' + id).addEventListener('click', () => telegramAction(action));
}

/* ================= 上传与打码 ================= */
const drop = $('#drop');
const fileInput = $('#fileInput');
const redactDialog = $('#redactDialog');
const redactCanvas = $('#redactCanvas');
const redactStage = $('#redactStage');
const redactOutline = $('#redactOutline');
let redaction = null;

function editableRaster(file) {
  return /^image\/(png|jpe?g|webp)$/i.test(file.type);
}

async function isStaticRaster(file) {
  const h = new DataView(await file.slice(0, 30).arrayBuffer());
  if (h.byteLength < 30) return false;
  if (/^image\/jpe?g$/i.test(file.type)) return h.getUint16(0) === 0xffd8;
  if (file.type === 'image/webp') {
    return h.getUint32(0) === 0x52494646 && h.getUint32(8) === 0x57454250 && (
      h.getUint32(12) === 0x56503820 ||
      h.getUint32(12) === 0x5650384c ||
      (h.getUint32(12) === 0x56503858 && !(h.getUint8(20) & 2))
    );
  }
  if (h.getUint32(0) !== 0x89504e47 || h.getUint32(4) !== 0x0d0a1a0a) return false;
  const png = new DataView(await file.arrayBuffer());
  // APNG requires an acTL chunk before the first IDAT.
  for (let off = 8; off + 12 <= png.byteLength;) {
    const len = png.getUint32(off);
    if (len > png.byteLength - off - 12) return false;
    const kind = png.getUint32(off + 4);
    if (kind === 0x6163544c) return false;
    if (kind === 0x49444154) return true;
    off += 12 + len;
  }
  return false;
}

function redactStatus(text = '') {
  const msg = $('#redactError');
  msg.textContent = text;
  msg.hidden = !text;
}

function redactControls() {
  const s = redaction;
  $('#redactBlack').setAttribute('aria-pressed', String(s.mode === 'black'));
  $('#redactPixel').setAttribute('aria-pressed', String(s.mode === 'pixel'));
  $('#redactUndo').disabled = $('#redactApply').disabled = s.saving || !s.regions.length;
  $('#redactApply').textContent = s.saving ? '正在处理…' : '应用打码';
}

function drawRedaction(canvas, s) {
  const ctx = canvas.getContext('2d');
  if (!ctx) throw new Error('画布不可用');
  const w = canvas.width;
  const h = canvas.height;
  ctx.clearRect(0, 0, w, h);
  ctx.imageSmoothingEnabled = true;
  ctx.drawImage(s.image, 0, 0, w, h);
  const sample = document.createElement('canvas');
  // Paint black last so overlapping mosaics cannot weaken an opaque cover.
  for (const mode of ['pixel', 'black']) {
    for (const r of s.regions) {
      if (r.mode !== mode) continue;
      const x = Math.floor(r.left * w);
      const y = Math.floor(r.top * h);
      const rw = Math.ceil(r.right * w) - x;
      const rh = Math.ceil(r.bottom * h) - y;
      if (mode === 'black') {
        ctx.fillStyle = '#000';
        ctx.fillRect(x, y, rw, rh);
        continue;
      }
      sample.width = Math.max(1, Math.floor(rw / Math.max(2, 24 * w / s.image.naturalWidth)));
      sample.height = Math.max(1, Math.floor(rh / Math.max(2, 24 * h / s.image.naturalHeight)));
      sample.getContext('2d').drawImage(canvas, x, y, rw, rh, 0, 0, sample.width, sample.height);
      ctx.clearRect(x, y, rw, rh);
      ctx.imageSmoothingEnabled = false;
      ctx.drawImage(sample, x, y, rw, rh);
    }
  }
}

async function openRedaction(q) {
  if (!q || redaction || uploading || !['waiting', 'error'].includes(q.status) || !editableRaster(q.file)) return;
  const quality = Number(uploadQuality.value);
  if (q.file.size > compressionInputMaxBytes) {
    toast('原文件超过 50 MB，请先缩小图片');
    return;
  }
  const s = redaction = { q, quality, image: new Image(), mode: 'black', regions: [], drag: null, saving: false };
  $('#redactOutputHint').textContent = quality === 100
    ? '当前输出：无损 PNG。想减小体积，可先调低上传页的压缩质量。'
    : `当前输出：${quality}% 画质，应用时直接压缩。`;
  redactStage.hidden = true;
  redactStatus('正在读取图片…');
  redactControls();
  redactDialog.showModal();
  try {
    if (!await isStaticRaster(q.file)) throw new Error('打码仅支持静态 JPG、PNG 和 WebP');
    if (redaction !== s) return;
    s.image.src = q.preview;
    await s.image.decode();
    if (redaction !== s) return;
    const w = s.image.naturalWidth;
    const h = s.image.naturalHeight;
    if (!w || !h || w * h > 50_000_000) throw new Error('图片超过 5000 万像素或无法读取');
    const scale = Math.min(1, 1400 / Math.max(w, h));
    redactCanvas.width = Math.max(1, Math.round(w * scale));
    redactCanvas.height = Math.max(1, Math.round(h * scale));
    redactStage.style.width = `min(100%, ${Math.min(w, 900)}px, ${50 * w / h}svh)`;
    drawRedaction(redactCanvas, s);
    redactStage.hidden = false;
    redactStatus();
  } catch (err) {
    if (redaction === s) redactStatus(err.message || '无法读取图片');
  }
}

function closeRedaction(restoreFocus = true) {
  const s = redaction;
  if (!s) return;
  redaction = null;
  s.image.removeAttribute('src');
  redactCanvas.width = 1;
  redactCanvas.height = 1;
  redactOutline.hidden = true;
  redactDialog.close();
  if (restoreFocus) $('#queue').querySelector('[data-qredact="' + s.q.qid + '"]')?.focus({ preventScroll: true });
}

function redactPoint(e) {
  const b = redactCanvas.getBoundingClientRect();
  const clamp = v => Math.max(0, Math.min(1, v));
  return { x: clamp((e.clientX - b.left) / b.width), y: clamp((e.clientY - b.top) / b.height) };
}

function redactRect(e, drag) {
  const p = redactPoint(e);
  return {
    left: Math.min(p.x, drag.x),
    top: Math.min(p.y, drag.y),
    right: Math.max(p.x, drag.x),
    bottom: Math.max(p.y, drag.y),
    mode: drag.mode
  };
}

redactCanvas.addEventListener('pointerdown', e => {
  const s = redaction;
  if (!s || s.drag || s.saving || e.button !== 0) return;
  s.drag = { ...redactPoint(e), id: e.pointerId, mode: s.mode };
  redactCanvas.setPointerCapture(e.pointerId);
});
redactCanvas.addEventListener('pointermove', e => {
  const drag = redaction?.drag;
  if (drag?.id !== e.pointerId) return;
  const r = redactRect(e, drag);
  Object.assign(redactOutline.style, {
    left: r.left * 100 + '%',
    top: r.top * 100 + '%',
    width: (r.right - r.left) * 100 + '%',
    height: (r.bottom - r.top) * 100 + '%'
  });
  redactOutline.hidden = false;
});
for (const event of ['pointerup', 'pointercancel', 'lostpointercapture']) {
  redactCanvas.addEventListener(event, e => {
    const s = redaction;
    const drag = s?.drag;
    if (drag?.id !== e.pointerId) return;
    s.drag = null;
    redactOutline.hidden = true;
    if (event !== 'pointerup') return;
    const r = redactRect(e, drag);
    if (r.right <= r.left || r.bottom <= r.top) return;
    s.regions.push(r);
    drawRedaction(redactCanvas, s);
    redactStatus();
    redactControls();
  });
}
for (const [id, mode] of [['redactBlack', 'black'], ['redactPixel', 'pixel']]) {
  $('#' + id).addEventListener('click', () => {
    if (redaction) {
      redaction.mode = mode;
      redactControls();
    }
  });
}
$('#redactUndo').addEventListener('click', () => {
  const s = redaction;
  if (!s || s.saving) return;
  s.regions.pop();
  drawRedaction(redactCanvas, s);
  redactStatus();
  redactControls();
});
$('#redactCancel').addEventListener('click', () => closeRedaction());
redactDialog.addEventListener('cancel', e => {
  e.preventDefault();
  closeRedaction();
});
$('#redactApply').addEventListener('click', async () => {
  const s = redaction;
  if (!s || s.saving || s.drag || !s.regions.length) return;
  s.saving = true;
  redactControls();
  redactStatus();
  const output = document.createElement('canvas');
  try {
    output.width = s.image.naturalWidth;
    output.height = s.image.naturalHeight;
    drawRedaction(output, s);
    const q = s.q;
    const base = q.name.replace(/\.[^.]+$/, '') + (q.redacted ? '' : '-打码');
    const file = await encodeCanvas(output, base, canvasOutputFormat(q.file, s.quality), s.quality);
    if (redaction !== s || !state.queue.includes(q)) return;
    if (file.size > uploadMaxBytes) throw new Error('打码结果超过 20 MB。未替换队列图片，请取消后调低压缩质量再试。');
    const preview = URL.createObjectURL(file);
    URL.revokeObjectURL(q.preview);
    Object.assign(q, {
      file,
      name: file.name,
      preview,
      sizeKB: file.size / 1024,
      status: 'waiting',
      progress: 0,
      redacted: true,
      encodedQuality: s.quality
    });
    renderQueue();
    closeRedaction();
    toast('已应用打码，点开始上传');
  } catch (err) {
    if (redaction === s) redactStatus(err.message || '打码失败，未替换队列中的图片');
  } finally {
    output.width = 1;
    output.height = 1;
    if (redaction === s) {
      s.saving = false;
      redactControls();
    }
  }
});

const uploadQuality = $('#uploadQuality');
uploadQuality.addEventListener('input', () => {
  const label = uploadQuality.value === '100' ? '原画质' : uploadQuality.value + '%';
  $('#uploadQualityValue').value = label;
  uploadQuality.setAttribute('aria-valuetext', label);
});

let seedN = 100;
function addFiles(fileList) {
  if (serverHasAuth && !getToken()) {
    promptAuthModal('请先输入口令登录图床');
    return;
  }
  if (redaction) return;
  if (uploading) {
    toast('当前批次上传中，请完成后再添加');
    return;
  }
  const files = [...fileList].filter(f => f.type.startsWith('image/'));
  const queued = state.queue.filter(q => q.status === 'waiting' || q.status === 'error').length;
  if (files.length + queued > 6) {
    toast('每批最多 6 张，当前待上传 ' + queued + ' 张；本次未添加');
    return;
  }
  const added = [];
  files.forEach(f => {
    if (!f.type.startsWith('image/')) return;
    const q = {
      qid: 'q' + (++seedN),
      file: f,
      name: f.name,
      sizeKB: f.size / 1024,
      status: 'waiting',
      progress: 0,
      preview: URL.createObjectURL(f)
    };
    state.queue.unshift(q);
    added.push(q);
  });
  if (added.length) {
    renderQueue();
    toast('已加入 ' + added.length + ' 张，点开始上传');
  } else {
    toast('没有可上传的图片文件');
  }
}

function renderQueue() {
  const box = $('#queue');
  const actions = $('#queueActions');
  const hasQueue = state.queue.length > 0;
  const pendingCount = state.queue.filter(q => q.status === 'waiting' || q.status === 'error').length;
  const doneCount = state.queue.filter(q => q.status === 'done').length;
  const hasPending = pendingCount > 0 || uploading;
  actions.hidden = !hasQueue;
  $('#uploadQualityPanel').hidden = !hasPending;
  uploadQuality.disabled = uploading;
  $('#uploadBtn').classList.toggle('is-idle', !hasPending);
  $('#queueClearBtn').disabled = uploading;
  if (hasQueue) {
    const totalKB = state.queue.reduce((s, q) => s + (q.sizeKB || 0), 0);
    $('#queueSummary').textContent = uploading
      ? `正在上传 · 已完成 ${doneCount}/${state.queue.length} 张 · 共 ${fmtSize(totalKB)}`
      : (pendingCount ? `待上传 ${pendingCount}/6 张 · 共 ${fmtSize(totalKB)}` : `已完成 ${doneCount} 张 · 共 ${fmtSize(totalKB)}`);
  } else {
    $('#queueSummary').textContent = '';
  }
  box.innerHTML = state.queue.map(q => {
    const st = q.status === 'done'
      ? '<span class="st ok">完成</span>'
      : q.status === 'error'
        ? '<span class="st err">失败</span>'
        : q.status === 'uploading'
          ? '<span class="st">' + (q.progress >= 98 ? '处理中' : q.progress + '%') + '</span>'
          : '<span class="st" style="color:var(--muted)">待上传</span>';
    const copyBtn = q.status === 'done' && q.result
      ? '<button type="button" class="mini" data-qcopy="' + q.qid + '">复制</button>'
      : '';
    const editBtn = !uploading && ['waiting', 'error'].includes(q.status) && editableRaster(q.file)
      ? '<button type="button" class="mini" data-qredact="' + q.qid + '">' + (q.redacted ? '继续打码' : '打码') + '</button>'
      : '';
    const meta = q.status === 'done'
      ? fmtSize(q.sizeKB) + ' → ' + fmtSize(q.result.sizeKB)
      : q.status === 'error'
        ? esc(q.error || '上传失败')
        : fmtSize(q.sizeKB) + (q.status === 'uploading' ? ' · ' + esc(q.phase || '准备上传') : '');
    return '<div class="qitem"><img src="' + q.preview + '" alt=""><div class="info"><div class="name">' + esc(q.name) + '</div><div class="meta">' + meta + '</div>' +
      (q.status === 'uploading' ? '<div class="bar"><i style="width:' + q.progress + '%"></i></div>' : '') +
      '</div><div class="qitem-actions">' + st + editBtn + copyBtn + '<button type="button" class="rm" data-rm="' + q.qid + '" aria-label="从队列移除">×</button></div></div>';
  }).join('');
}

$('#queue').addEventListener('click', e => {
  const edit = e.target.closest('[data-qredact]');
  if (edit) {
    openRedaction(state.queue.find(q => q.qid === edit.dataset.qredact));
    return;
  }
  const cp = e.target.closest('[data-qcopy]');
  if (cp) {
    const q = state.queue.find(it => it.qid === cp.dataset.qcopy);
    if (q?.result) {
      copyText(formatLink({ name: q.name, url: q.result.proxyUrl || q.result.url, proxyUrl: q.result.proxyUrl || q.result.url }, state.fmt), '已复制链接');
    }
    return;
  }
  const rm = e.target.closest('[data-rm]');
  if (!rm) return;
  const q = state.queue.find(it => it.qid === rm.dataset.rm);
  if (!q) return;
  if (q.status === 'uploading') {
    toast('正在上传，请等待完成后移除');
    return;
  }
  URL.revokeObjectURL(q.preview);
  state.queue = state.queue.filter(it => it !== q);
  renderQueue();
});

$('#queueClearBtn').addEventListener('click', () => {
  if (uploading) {
    toast('正在上传，请等待完成后清空');
    return;
  }
  state.queue.forEach(q => URL.revokeObjectURL(q.preview));
  state.queue = [];
  renderQueue();
});

drop.addEventListener('click', () => {
  if (serverHasAuth && !getToken()) {
    promptAuthModal('请先输入口令登录图床');
    return;
  }
  fileInput.click();
});
drop.addEventListener('keydown', e => {
  if (e.key === 'Enter' || e.key === ' ') {
    e.preventDefault();
    if (serverHasAuth && !getToken()) {
      promptAuthModal('请先输入口令登录图床');
      return;
    }
    fileInput.click();
  }
});
fileInput.addEventListener('change', () => {
  addFiles(fileInput.files);
  fileInput.value = '';
});
['dragenter', 'dragover'].forEach(ev => drop.addEventListener(ev, e => { e.preventDefault(); drop.classList.add('active'); }));
['dragleave', 'drop'].forEach(ev => drop.addEventListener(ev, e => { e.preventDefault(); drop.classList.remove('active'); }));
drop.addEventListener('drop', e => {
  e.preventDefault();
  drop.classList.remove('active');
  if (serverHasAuth && !getToken()) {
    promptAuthModal('请先输入口令登录图床');
    return;
  }
  addFiles(e.dataTransfer.files);
});
document.addEventListener('paste', e => {
  if (redaction || !['upload', 'gallery'].includes(state.page)) return;
  if (serverHasAuth && !getToken()) {
    promptAuthModal('请先输入口令登录图床');
    return;
  }
  const imgs = [...(e.clipboardData && e.clipboardData.items || [])]
    .filter(i => i.type.startsWith('image/'))
    .map(i => i.getAsFile())
    .filter(Boolean);
  if (imgs.length) {
    showPage('upload');
    addFiles(imgs);
  }
});

async function runUploadPool(pending, concurrency, upload) {
  let next = 0;
  await Promise.all(Array.from({ length: Math.min(concurrency, pending.length) }, async () => {
    while (next < pending.length) {
      const item = pending[next++];
      await upload(item);
    }
  }));
}

let uploading = false;
$('#uploadBtn').addEventListener('click', async () => {
  if (serverHasAuth && !getToken()) {
    promptAuthModal('请先输入口令登录图床');
    return;
  }
  if (uploading || redaction) return;
  const uploadToken = getToken();
  const quality = Number(uploadQuality.value);
  const pending = state.queue.filter(q => q.status === 'waiting' || q.status === 'error');
  if (!pending.length) {
    toast(state.queue.length ? '队列里没有待上传的图片' : '先选择或拖入图片');
    return;
  }
  if (pending.length > 6) {
    toast('每批最多 6 张，请先移除多余图片');
    return;
  }
  uploading = true;
  $('#uploadBtn').disabled = true;
  $('#uploadBtn').textContent = '上传中…';
  try {
    await runUploadPool(pending, 3, async q => {
      if (getToken() !== uploadToken || !state.queue.includes(q)) return;
      const historyVersion = recentWriteVersion;
      q.status = 'uploading';
      q.progress = 0;
      renderQueue();
      try {
        // Redaction already encoded at this quality; don't add another lossy pass.
        const nextQuality = quality >= (q.encodedQuality ?? 100) ? 100 : quality;
        q.result = await PicAPI.upload(q.file, (p, phase) => {
          q.progress = p;
          q.phase = phase;
          renderQueue();
        }, nextQuality);
        if (getToken() !== uploadToken || !state.queue.includes(q)) {
          q.status = 'error';
          q.error = '登录信息已变更，请刷新图库确认结果';
          return;
        }
        q.status = 'done';
        q.progress = 100;
        const item = {
          id: q.result.key,
          key: q.result.key,
          name: q.result.key.split('/').pop(),
          backend: q.result.backend,
          sizeKB: q.result.sizeKB,
          origKB: q.result.origKB,
          ts: Date.now(),
          url: q.result.proxyUrl || q.result.url,
          directUrl: q.result.directUrl,
          proxyUrl: q.result.proxyUrl,
          thumbUrl: q.result.thumbUrl
        };
        items = items.filter(it => it.id !== item.id);
        items.unshift(item);
        void rememberLocalUpload(item, q.file, historyVersion);
        renderGallery();
      } catch (err) {
        q.status = 'error';
        q.error = err.message;
      }
      renderQueue();
    });
  } finally {
    uploading = false;
    $('#uploadBtn').disabled = false;
    $('#uploadBtn').textContent = '开始上传';
    renderQueue();
  }
  if (getToken() !== uploadToken) return;
  const done = pending.filter(q => q.status === 'done');
  if (done.length) toast('上传完成 ' + done.length + ' 张');
  else toast('上传失败，请检查文件后重试');
});

/* ================= 复制格式 ================= */
function formatLink(item, fmt) {
  const name = item.name.replace(/\.[^.]+$/, '');
  if (fmt === 'md') return markdownImage(item);
  if (fmt === 'bb') return '[img]' + item.url + '[/img]';
  if (fmt === 'html') return '<img src="' + esc(item.url) + '" alt="' + esc(name) + '" />';
  return item.url;
}

/* ================= Browser-local upload history ================= */
const RECENT_DB = 'pic-tg-local-history';
const RECENT_MAX = 200;
let recentDBPromise = null;
let localRecent = [];
let recentGeneration = 0;
let recentVisible = 12;
const recentObjectURLs = new Map();
const recentRemovedVersions = new Map();
let recentWriteVersion = 0;
let recentClearVersion = 0;

function localWriteIsCurrent(key, version) {
  return version >= recentClearVersion && version >= (recentRemovedVersions.get(key) || 0);
}

function openRecentDB() {
  if (recentDBPromise) return recentDBPromise;
  recentDBPromise = new Promise((resolve, reject) => {
    if (!window.indexedDB) {
      reject(new Error('浏览器不支持本地记录'));
      return;
    }
    const request = indexedDB.open(RECENT_DB, 1);
    request.onupgradeneeded = () => {
      const store = request.result.createObjectStore('uploads', { keyPath: 'key' });
      store.createIndex('ts', 'ts');
    };
    request.onsuccess = () => {
      const db = request.result;
      db.onversionchange = () => {
        db.close();
        recentDBPromise = null;
      };
      resolve(db);
    };
    request.onerror = () => reject(request.error);
    request.onblocked = () => reject(new Error('本地记录暂时不可用'));
  }).catch(error => {
    recentDBPromise = null;
    throw error;
  });
  return recentDBPromise;
}

async function recentTransaction(mode, run) {
  const db = await openRecentDB();
  return new Promise((resolve, reject) => {
    const tx = db.transaction('uploads', mode);
    let value;
    try {
      run(tx.objectStore('uploads'), result => { value = result; });
    } catch (error) {
      tx.abort();
      reject(error);
      return;
    }
    tx.oncomplete = () => resolve(value);
    tx.onabort = tx.onerror = () => reject(tx.error || new Error('本地记录保存失败'));
  });
}

function clearRecentMemory() {
  recentClearVersion = ++recentWriteVersion;
  recentRemovedVersions.clear();
  recentGeneration++;
  for (const url of recentObjectURLs.values()) URL.revokeObjectURL(url);
  recentObjectURLs.clear();
  localRecent = [];
  recentVisible = 12;
  $('#recentGrid').replaceChildren();
  $('#recentCard').hidden = true;
  $('#recentEmpty').hidden = false;
  $('#recentStatus').hidden = true;
}

async function upgradeRecentLinks(rows, generation, token) {
  const legacy = rows.filter(row => /^telegram\//.test(row.key) && !new URL(row.proxyUrl || row.url, location.origin).pathname.startsWith('/i/'));
  if (!legacy.length) return;
  try {
    const response = await fetch('/api/share-links', {
      method: 'POST',
      headers: authHeaders({ 'Content-Type': 'application/json' }, token),
      body: JSON.stringify({ keys: legacy.map(row => row.key) })
    });
    if (!response.ok) return;
    const result = await response.json();
    if (result.code !== 200 || generation !== recentGeneration || token !== getToken()) return;
    await recentTransaction('readwrite', store => {
      if (generation !== recentGeneration || token !== getToken()) return;
      for (const row of legacy) {
        const url = result.data[row.key];
        if (!url) continue;
        const request = store.get(row.key);
        request.onsuccess = () => {
          const current = request.result;
          if (generation !== recentGeneration || token !== getToken() || !current || current.ts !== row.ts) return;
          current.url = url;
          current.proxyUrl = url;
          store.put(current);
          row.url = url;
          row.proxyUrl = url;
        };
      }
    });
  } catch (error) {
    /* Offline history remains usable; retry on the next read. */
  }
}

async function loadLocalRecent() {
  if (document.body.classList.contains('auth-locked')) return;
  const generation = ++recentGeneration;
  const token = getToken();
  try {
    const rows = await recentTransaction('readonly', (store, result) => {
      const request = store.getAll();
      request.onsuccess = () => result(request.result);
    });
    if (generation !== recentGeneration || token !== getToken()) return;
    await upgradeRecentLinks(rows, generation, token);
    if (generation !== recentGeneration || token !== getToken()) return;
    const fresh = rows.filter(row => row.thumbnail instanceof Blob).sort((a, b) => b.ts - a.ts);
    const previous = new Map(localRecent.map(row => [row.key, row]));
    for (const [key, url] of recentObjectURLs) {
      const row = fresh.find(item => item.key === key);
      const old = previous.get(key);
      if (!row || !old || row.ts !== old.ts) {
        URL.revokeObjectURL(url);
        recentObjectURLs.delete(key);
      }
    }
    localRecent = fresh;
    renderRecent();
  } catch (error) {
    if (generation === recentGeneration) {
      $('#recentStatus').textContent = '本地记录不可用';
      $('#recentStatus').hidden = false;
    }
  }
}

async function cachedThumbnail(file) {
  const url = URL.createObjectURL(file);
  try {
    const image = await new Promise((resolve, reject) => {
      const img = new Image();
      img.onload = () => resolve(img);
      img.onerror = () => reject(new Error('缩略图生成失败'));
      img.src = url;
    });
    const scale = Math.min(1, 240 / image.naturalWidth, 180 / image.naturalHeight);
    const canvas = document.createElement('canvas');
    canvas.width = Math.max(1, Math.round(image.naturalWidth * scale));
    canvas.height = Math.max(1, Math.round(image.naturalHeight * scale));
    canvas.getContext('2d').drawImage(image, 0, 0, canvas.width, canvas.height);
    return await new Promise((resolve, reject) => {
      canvas.toBlob(blob => (blob ? resolve(blob) : reject(new Error('缩略图生成失败'))), 'image/webp', 0.82);
    });
  } finally {
    URL.revokeObjectURL(url);
  }
}

async function rememberLocalUpload(item, file, version = recentWriteVersion) {
  const generation = authGeneration;
  const token = getToken();
  try {
    const thumbnail = await cachedThumbnail(file);
    if (generation !== authGeneration || token !== getToken() || !localWriteIsCurrent(item.key, version)) return;
    const record = {
      key: item.key,
      id: item.key,
      name: file.name || item.name,
      url: item.proxyUrl || item.url,
      proxyUrl: item.proxyUrl || item.url,
      sizeKB: item.sizeKB,
      origKB: item.origKB,
      ts: Date.now(),
      thumbnail
    };
    await recentTransaction('readwrite', store => {
      if (generation !== authGeneration || token !== getToken() || !localWriteIsCurrent(item.key, version)) return;
      store.put(record);
      let count = 0;
      const request = store.index('ts').openCursor(null, 'prev');
      request.onsuccess = () => {
        const cursor = request.result;
        if (!cursor) return;
        if (++count > RECENT_MAX) cursor.delete();
        cursor.continue();
      };
    });
    if (generation !== authGeneration || token !== getToken() || !localWriteIsCurrent(item.key, version)) return;
    await loadLocalRecent();
  } catch (error) {
    if (generation === authGeneration) {
      $('#recentStatus').textContent = '已上传，但本地记录未保存';
      $('#recentStatus').hidden = false;
    }
  }
}

function recentUploads() {
  return localRecent;
}

function recentLinks() {
  return localRecent.map(item => formatLink(item, state.fmt)).join('\n');
}

function renderRecent() {
  const rows = recentUploads();
  $('#countRecent').textContent = rows.length;
  $('#recentCard').hidden = !rows.length;
  $('#recentEmpty').hidden = !!rows.length;
  if (!rows.length) {
    $('#recentGrid').replaceChildren();
    return;
  }
  $('#recentCount').textContent = rows.length + ' 张';
  syncKeyedCards($('#recentGrid'), rows.slice(0, recentVisible).map(item => {
    let thumbnail = recentObjectURLs.get(item.key);
    if (!thumbnail) {
      thumbnail = URL.createObjectURL(item.thumbnail);
      recentObjectURLs.set(item.key, thumbnail);
    }
    return {
      key: item.key,
      html: '<article class="recent-item"><button class="recent-thumb" data-recent-preview="' + esc(item.key) + '" aria-label="预览 ' + esc(item.name) + '"><img src="' + thumbnail + '" alt="' + esc(item.name) + '"></button><div class="recent-body"><span class="recent-name" title="' + esc(item.name) + '">' + esc(item.name) + '</span><small>' + fmtSize(item.sizeKB) + '</small><div class="recent-actions"><button class="mini" data-recent-copy="' + esc(item.key) + '">复制</button><button class="mini" data-recent-remove="' + esc(item.key) + '" aria-label="移除 ' + esc(item.name) + ' 的本地记录">移除</button></div></div></article>'
    };
  }));
  $('#recentMore').hidden = rows.length <= recentVisible;
}

$('#recentGrid').addEventListener('click', event => {
  const copy = event.target.closest('[data-recent-copy]');
  const remove = event.target.closest('[data-recent-remove]');
  const preview = event.target.closest('[data-recent-preview]');
  if (copy) {
    const item = localRecent.find(it => it.key === copy.dataset.recentCopy);
    if (item) copyText(formatLink(item, state.fmt), '已复制');
  } else if (remove) {
    forgetLocalRecent([remove.dataset.recentRemove]);
  } else if (preview) {
    const item = localRecent.find(it => it.key === preview.dataset.recentPreview);
    if (!item) return;
    $('#recentZoomImg').src = recentObjectURLs.get(item.key);
    $('#recentZoomImg').alt = item.name;
    $('#recentZoom').hidden = false;
    $('#recentZoom').classList.add('show');
  }
});

async function forgetLocalRecent(keys) {
  const version = ++recentWriteVersion;
  recentGeneration++;
  for (const key of keys) recentRemovedVersions.set(key, version);
  try {
    await recentTransaction('readwrite', store => keys.forEach(key => store.delete(key)));
    await loadLocalRecent();
  } catch (error) {
    toast('本地记录移除失败');
  }
}

async function clearLocalRecent() {
  recentClearVersion = ++recentWriteVersion;
  recentRemovedVersions.clear();
  recentGeneration++;
  await recentTransaction('readwrite', store => store.clear());
  await loadLocalRecent();
}

$('#recentClear').addEventListener('click', async () => {
  if (!await confirmDialog('清空本机记录', '只清除此浏览器的上传记录，不删除图片。', '清空记录')) return;
  try {
    await clearLocalRecent();
    toast('本机记录已清空');
  } catch (error) {
    toast('清空失败，请重试');
  }
});
$('#recentMore').addEventListener('click', () => {
  recentVisible += 12;
  renderRecent();
});
$('#fmtTabs').addEventListener('click', event => {
  const button = event.target.closest('button');
  if (!button) return;
  setDefaultFormat(button.dataset.f);
});
$('#copyFmtBtn').addEventListener('click', () => {
  if (localRecent.length) copyText(recentLinks(), '已复制 ' + localRecent.length + ' 张图片链接');
});
/* ================= End browser-local history ================= */

/* ================= 图库 ================= */
let galleryRequest = 0;

async function refreshGallery() {
  const request = ++galleryRequest;
  const token = getToken();
  try {
    const fresh = await PicAPI.list();
    if (request !== galleryRequest || getToken() !== token) return;
    items = fresh;
    renderGallery();
  } catch (err) {
    if (request === galleryRequest && getToken() === token) toast(err.message);
  }
}

function filteredItems() {
  const kw = $('#gallerySearch').value.trim().toLowerCase();
  const list = items.filter(it => !kw || it.name.toLowerCase().includes(kw) || imageDate(it.ts).includes(kw));
  const sort = state.sort;
  list.sort((a, b) => (sort === 'old' ? a.ts - b.ts : sort === 'big' ? b.sizeKB - a.sizeKB : sort === 'small' ? a.sizeKB - b.sizeKB : b.ts - a.ts));
  return list;
}

function toggleGalleryCard(id) {
  if (!state.selectMode) return;
  state.selected.has(id) ? state.selected.delete(id) : state.selected.add(id);
  updateGallerySelection();
}

$('#grid').addEventListener('click', event => {
  const card = event.target.closest('[data-card]');
  if (!card) return;
  if (state.selectMode) {
    event.preventDefault();
    toggleGalleryCard(card.dataset.card);
    return;
  }
  const copy = event.target.closest('[data-copy]');
  const remove = event.target.closest('[data-del]');
  if (copy) {
    const item = items.find(it => it.id === copy.dataset.copy);
    if (item) copyText(formatLink(item, state.fmt), '已复制链接');
  } else if (remove) {
    deleteItems([remove.dataset.del]);
  } else if (event.target.closest('[data-open]')) {
    openInspector(card.dataset.card);
  }
});
$('#grid').addEventListener('keydown', event => {
  if (!state.selectMode || !event.target.matches('[data-card]') || !['Enter', ' '].includes(event.key)) return;
  event.preventDefault();
  toggleGalleryCard(event.target.dataset.card);
});

function imageDate(ts) {
  return new Intl.DateTimeFormat('en-CA', { year: 'numeric', month: '2-digit', day: '2-digit' }).format(new Date(ts));
}

function dateFolders(list) {
  const map = new Map();
  for (const it of list) {
    const date = imageDate(it.ts);
    if (!map.has(date)) map.set(date, { date, count: 0, sizeKB: 0 });
    const g = map.get(date);
    g.count++;
    g.sizeKB += it.sizeKB;
  }
  return [...map.values()].sort((a, b) => b.date.localeCompare(a.date));
}

function setGalleryFolder(folder) {
  state.folder = folder;
  state.selected.clear();
  state.selectMode = false;
  closeInspector();
  renderGallery();
}

function setSortMenuOpen(open) {
  const wrap = $('#gallerySort');
  const btn = $('#gallerySortBtn');
  const menu = $('#gallerySortMenu');
  if (!wrap || !btn || !menu) return;
  const next = !!open;
  wrap.classList.toggle('open', next);
  btn.setAttribute('aria-expanded', String(next));
  menu.hidden = !next;
}

function setGallerySort(val, { render = true, close = true } = {}) {
  if (!Object.hasOwn(SORT_OPTIONS, val)) val = 'new';
  const changed = state.sort !== val;
  state.sort = val;
  $('#gallerySortLabel').textContent = SORT_OPTIONS[val];
  $$('#gallerySortMenu [data-sort]').forEach(opt => {
    const on = opt.dataset.sort === val;
    opt.classList.toggle('on', on);
    opt.setAttribute('aria-selected', String(on));
  });
  if (close) setSortMenuOpen(false);
  if (changed && render) renderGallery();
}

$('#gallerySearch').addEventListener('input', () => renderGallery());
$('#gallerySortBtn').addEventListener('click', () => setSortMenuOpen($('#gallerySortMenu').hidden));
$('#gallerySortBtn').addEventListener('keydown', e => {
  if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
    e.preventDefault();
    setSortMenuOpen(true);
    const active = $('#gallerySortMenu .sort-option.on') || $('#gallerySortMenu .sort-option');
    active?.focus();
  }
});
$('#gallerySortMenu').addEventListener('click', e => {
  const opt = e.target.closest('[data-sort]');
  if (!opt) return;
  setGallerySort(opt.dataset.sort);
  $('#gallerySortBtn').focus({ preventScroll: true });
});
$('#gallerySortMenu').addEventListener('keydown', e => {
  const opts = $$('#gallerySortMenu [data-sort]');
  const idx = opts.indexOf(document.activeElement);
  if (e.key === 'ArrowDown') { e.preventDefault(); opts[(idx + 1) % opts.length]?.focus(); }
  else if (e.key === 'ArrowUp') { e.preventDefault(); opts[(idx - 1 + opts.length) % opts.length]?.focus(); }
  else if (e.key === 'Home') { e.preventDefault(); opts[0]?.focus(); }
  else if (e.key === 'End') { e.preventDefault(); opts.at(-1)?.focus(); }
  else if (e.key === 'Tab') { setSortMenuOpen(false); }
  else if (e.key === 'Escape') {
    e.preventDefault();
    e.stopPropagation();
    setSortMenuOpen(false);
    $('#gallerySortBtn').focus({ preventScroll: true });
  }
});
document.addEventListener('pointerdown', e => {
  if (!$('#gallerySortMenu').hidden && !e.target.closest('#gallerySort')) setSortMenuOpen(false);
});

async function deleteItems(ids) {
  if (serverHasAuth && !getToken()) {
    promptAuthModal('请先输入口令登录');
    return;
  }
  const targets = items.filter(it => ids.includes(it.id));
  if (!targets.length) return;
  const ok = await confirmDialog('删除图片', '确定删除 ' + targets.length + ' 张图片吗？会尝试删除 TG 原图；即使 TG 无法删除，也会移除图库记录并停用图片链接。已缓存的副本不处理。', '删除');
  if (!ok) return;
  try {
    const keys = targets.map(it => it.key || it.id);
    const result = await PicAPI.batchRemove(keys);
    const deleted = new Set(result.deleted_keys || []);
    items = items.filter(it => !deleted.has(it.key || it.id));
    deleted.forEach(id => state.selected.delete(id));
    await forgetLocalRecent([...deleted]);
    renderGallery();
    toast(result.errors?.length ? '已删除 ' + deleted.size + ' 张，其余删除失败，请重试' : '已删除 ' + deleted.size + ' 张');
  } catch (err) {
    toast('删除失败: ' + err.message);
  }
}

$('#selectModeBtn').addEventListener('click', () => {
  if (serverHasAuth && !getToken()) {
    promptAuthModal('请先输入口令登录');
    return;
  }
  state.selectMode = !state.selectMode;
  $('#selectModeBtn').textContent = state.selectMode ? '完成' : '选择';
  $('#batchbar').classList.toggle('show', state.selectMode);
  if (!state.selectMode) state.selected.clear();
  updateGallerySelection();
});
$('#batchCancelBtn').addEventListener('click', () => $('#selectModeBtn').click());
$('#batchCopyBtn').addEventListener('click', () => {
  if (serverHasAuth && !getToken()) {
    promptAuthModal('请先输入口令登录');
    return;
  }
  const chosen = items.filter(it => state.selected.has(it.id));
  if (!chosen.length) { toast('先勾选图片'); return; }
  copyText(chosen.map(it => it.url).join('\n'), '已复制 ' + chosen.length + ' 条直链');
});
$('#batchCopyMD').addEventListener('click', () => {
  if (serverHasAuth && !getToken()) {
    promptAuthModal('请先输入口令登录');
    return;
  }
  const chosen = items.filter(it => state.selected.has(it.id));
  if (!chosen.length) { toast('先勾选图片'); return; }
  copyText(chosen.map(markdownImage).join('\n'), '已复制 ' + chosen.length + ' 条 Markdown 链接');
});
$('#batchDeleteBtn').addEventListener('click', () => {
  if (serverHasAuth && !getToken()) {
    promptAuthModal('请先输入口令登录');
    return;
  }
  if (!state.selected.size) { toast('先勾选图片'); return; }
  deleteItems([...state.selected]);
});

/* ================= 本地预览 & Markdown 转义 ================= */
function markdownImage(it) {
  const alt = String(it.name || '图片').replace(/[\\[\]<>]/g, '\\$&').replace(/[\r\n]/g, ' ');
  const url = (it.proxyUrl || it.url).replace(/[<>()\s]/g, c => encodeURIComponent(c));
  return '![' + alt + '](' + url + ')';
}

function closeRecentZoom() {
  $('#recentZoom').classList.remove('show');
  $('#recentZoom').hidden = true;
  $('#recentZoomImg').removeAttribute('src');
}

$('#recentZoomClose').addEventListener('click', closeRecentZoom);
$('#recentZoom').addEventListener('click', e => {
  if (e.target === $('#recentZoom') || e.target === $('#recentZoomImg')) closeRecentZoom();
});
document.addEventListener('keydown', e => {
  if (e.key === 'Escape') {
    if ($('#confirmOverlay').classList.contains('show')) {
      closeConfirm(false);
      return;
    }
    closeRecentZoom();
    closeAuthModal();
    closePwdModal();
  }
});

/* ================= API 页 ================= */
let tokenShown = false;
let apiKeyValue = '';
let apiKeyRequest = 0;

function maskToken(tok) {
  return tok ? '••••••••••••••••' : '尚未加载';
}

async function apiKeyRequestToServer(method = 'GET') {
  const res = await authFetch('/api/admin/api-key', {
    method,
    headers: authHeaders(),
    cache: 'no-store'
  });
  const json = await res.json();
  if (!res.ok || json.code !== 200) throw new Error(json.msg || 'API Key 获取失败');
  return json.data.api_key;
}

async function loadAPIKey() {
  if (serverHasAuth && !getToken()) return;
  const request = ++apiKeyRequest;
  const token = getToken();
  try {
    const key = await apiKeyRequestToServer();
    if (request !== apiKeyRequest || token !== getToken()) return;
    apiKeyValue = key;
    updateTokenUI();
  } catch (e) {
    if (request === apiKeyRequest) {
      apiKeyValue = '';
      updateTokenUI();
      $('#tokenText').textContent = e.message;
    }
  }
}

function updateTokenUI() {
  const loggedIn = !serverHasAuth || !!getToken();
  const key = loggedIn ? apiKeyValue : '';
  const origin = location.origin;
  $('#tokenText').textContent = !loggedIn ? '请先登录' : key ? (tokenShown ? key : maskToken(key)) : '打开 API 页加载独立 Key';
  $('#tokenText').style.cursor = 'default';
  $('#tokenText').onclick = null;
  $('#tokenShowBtn').disabled = !key;
  $('#tokenCopyBtn').disabled = !key;
  $('#tokenRegenBtn').disabled = !loggedIn;
  $('#tokenShowBtn').textContent = tokenShown ? '隐藏' : '显示';
  $('#apiUploadEndpoint').textContent = 'POST ' + origin + '/api/upload';
  $('#apiCurlCode').textContent = [
    '# 上传：multipart 字段 file，返回 data.key 和 data.url',
    `curl -H "Authorization: Bearer $API_KEY" -F "file=@photo.jpg" ${origin}/api/upload`,
    '',
    '# 列表：支持 limit、offset、q',
    `curl -H "Authorization: Bearer $API_KEY" '${origin}/api/objects?limit=50&offset=0'`,
    '',
    '# 下载：将 <key> 替换为上传返回的完整 data.key',
    `curl -H "Authorization: Bearer $API_KEY" '${origin}/api/download/<key>' -o photo.jpg`,
    '',
    '# 删除：尝试删除 TG 原图；即使 TG 无法删除也会移除图库索引并停用源站链接',
    `curl -X DELETE -H "Authorization: Bearer $API_KEY" -H 'Content-Type: application/json' -d '{"key":"<key>"}' ${origin}/api/objects`
  ].join('\n');
  $('#picgoCode').textContent = JSON.stringify({
    url: origin + '/api/upload',
    method: 'POST',
    headers: { Authorization: 'Bearer <API Key>' },
    body: { file: '$file$' },
    urlPath: 'data.url'
  }, null, 2);
}

$('#tokenShowBtn').addEventListener('click', () => {
  tokenShown = !tokenShown;
  updateTokenUI();
});
$('#tokenCopyBtn').addEventListener('click', () => {
  if (apiKeyValue) copyText(apiKeyValue, '已复制 API Key');
});
$('#tokenRegenBtn').addEventListener('click', async () => {
  if (serverHasAuth && !getToken()) { promptAuthModal(); return; }
  if (!await confirmDialog('重新生成 API Key', '旧 Key 会立即失效，外部客户端需更新。网页登录口令不变。', '重新生成')) return;
  const token = getToken();
  const request = ++apiKeyRequest;
  $('#tokenRegenBtn').disabled = true;
  try {
    const key = await apiKeyRequestToServer('POST');
    if (request !== apiKeyRequest || token !== getToken()) return;
    apiKeyValue = key;
    tokenShown = false;
    updateTokenUI();
    toast('API Key 已更新，登录口令不变');
  } catch (e) {
    if (request === apiKeyRequest) {
      updateTokenUI();
      toast(e.message);
    }
  }
});
$('#picgoCopyBtn').addEventListener('click', () => {
  if (serverHasAuth && !getToken()) { promptAuthModal(); return; }
  copyText($('#picgoCode').textContent, '已复制 PicGo 配置');
});

/* ================= Photos 工作区与详情弹窗 ================= */
let activeInspectorId = null;
let inspectorFormat = savedFmt;
let gridDensity = 'cozy';

function galleryViewItems() {
  let list = filteredItems();
  if (state.folder !== 'all' && state.folder !== 'root') {
    list = list.filter(it => imageDate(it.ts) === state.folder);
  }
  return list;
}

function showPage(name) {
  if (!PAGES.includes(name)) name = 'upload';
  if (pageInitialized && state.page === name) return;
  pageInitialized = true;
  state.page = name;
  $$('.view').forEach(v => v.classList.toggle('on', v.id === 'page-' + name));
  $$('[data-page]').forEach(b => {
    b.classList.toggle('on', b.dataset.page === name);
    b.setAttribute('aria-current', b.dataset.page === name ? 'page' : 'false');
  });
  if (location.hash !== '#' + name) history.replaceState(null, '', '#' + name);
  const gallery = name === 'gallery';
  $('#galleryControls').hidden = !gallery;
  $('#densitySeg').hidden = !gallery;
  $('#selectModeBtn').hidden = !gallery;
  closeInspector();
  setSortMenuOpen(false);
  if (!gallery) {
    state.selectMode = false;
    state.selected.clear();
    $('#batchbar').classList.remove('show');
  }
  $('#viewTitle').textContent = ({ gallery: '图片', upload: '上传图片', api: 'API 与接入', settings: '偏好设置' })[name];
  $('#viewSub').textContent = ({
    gallery: '正在加载',
    upload: '每批最多 6 张 · 3 路并行 · 单张最大 20 MB',
    api: '上传、下载与客户端接入',
    settings: '外观、账号与 Telegram 存储'
  })[name];
  window.scrollTo({ top: 0 });
  if (gallery) { renderGallery(); refreshGallery(); }
  if (name === 'upload') loadLocalRecent();
  if (name === 'api') { updateTokenUI(); loadAPIKey(); }
  if (name === 'settings') refreshStatus();
}

function tileRecord(it) {
  const timeText = new Intl.DateTimeFormat(undefined, { hour: '2-digit', minute: '2-digit', hour12: false }).format(new Date(it.ts));
  return {
    key: it.id,
    html: `<article class="tile" data-card="${esc(it.id)}"><img loading="lazy" decoding="async" src="${esc(it.thumbUrl || it.proxyUrl || it.url)}" alt="${esc(it.name)}"/><button class="tile-open" type="button" data-open="${esc(it.id)}" aria-label="查看 ${esc(it.name)}"></button><div class="tile-scrim"><div class="tile-top"><span class="tile-badge">${esc(fmtSize(it.sizeKB))}</span></div><div class="tile-bottom"><span class="tile-caption"><small>${esc(timeText)}</small></span><button class="tile-quick-copy" type="button" data-copy="${esc(it.id)}">复制</button></div></div></article>`
  };
}

function renderGallery() {
  const grid = $('#grid');
  if (document.body.classList.contains('auth-locked')) {
    grid.replaceChildren();
    return;
  }
  const list = galleryViewItems();
  const sort = state.sort;
  let groups = [];
  if (sort === 'new' || sort === 'old') {
    const dates = dateFolders(list);
    if (sort === 'old') dates.reverse();
    groups = dates.map(g => ({ date: g.date, list: list.filter(it => imageDate(it.ts) === g.date) }));
  } else if (list.length) {
    groups = [{ date: '', list }];
  }
  const existing = new Map([...grid.children].map(n => [n.dataset.date, n]));
  const retained = new Set();
  groups.forEach((group, index) => {
    let node = existing.get(group.date);
    if (!node) {
      node = document.createElement('section');
      node.className = 'date-group';
      node.dataset.date = group.date;
      node.innerHTML = '<div class="date-group-head"><h2></h2><span></span></div><div class="photo-grid"></div>';
    }
    node.querySelector('.date-group-head').hidden = !group.date;
    const title = node.querySelector('h2');
    const count = node.querySelector('.date-group-head span');
    const label = group.list.length + ' 张';
    if (title.textContent !== group.date) title.textContent = group.date;
    if (count.textContent !== label) count.textContent = label;
    const tiles = node.querySelector('.photo-grid');
    tiles.classList.toggle('compact', gridDensity === 'compact');
    syncKeyedCards(tiles, group.list.map(tileRecord));
    if (grid.children[index] !== node) grid.insertBefore(node, grid.children[index] || null);
    retained.add(node);
  });
  for (const node of [...grid.children]) {
    if (!retained.has(node)) node.remove();
  }
  $('#galleryEmpty').hidden = !!list.length;
  const total = items.reduce((sum, it) => sum + it.sizeKB, 0);
  const meta = items.length + ' 张 · ' + fmtSize(total);
  $('#storageInfo').textContent = meta;
  $('#sbStorageMeta').textContent = meta;
  $('#countAll').textContent = items.length;
  $('#countRecent').textContent = localRecent.length;
  if (state.page === 'gallery') {
    $('#viewTitle').textContent = state.folder === 'all' || state.folder === 'root' ? '图片' : state.folder;
    $('#viewSub').textContent = list.length + ' 张图片 · ' + fmtSize(list.reduce((sum, it) => sum + it.sizeKB, 0));
  }
  const dates = dateFolders(items);
  syncKeyedCards($('#sidebarDates'), dates.map(g => ({
    key: g.date,
    html: `<button class="sb-item" type="button" data-date="${g.date}"><span>${g.date}</span><em class="sb-count">${g.count}</em></button>`
  })));
  syncKeyedCards($('#mobileDateStrip'), [
    { key: 'all', html: '<button class="date-chip" type="button" data-date="all">全部</button>' },
    ...dates.map(g => ({
      key: g.date,
      html: `<button class="date-chip" type="button" data-date="${g.date}" aria-label="${g.date}，${g.count} 张">${g.date.slice(5)} (${g.count})</button>`
    }))
  ]);
  $$('[data-date]').forEach(b => {
    const on = b.dataset.date === state.folder;
    b.classList.toggle('on', on);
    b.setAttribute('aria-pressed', String(on));
  });
  updateGallerySelection();
  if (activeInspectorId && !items.some(it => it.id === activeInspectorId)) closeInspector();
}

function updateGallerySelection() {
  $('#grid').classList.toggle('selecting', state.selectMode);
  $('#batchInfo').textContent = '已选 ' + state.selected.size + ' 项';
  $('#batchbar').classList.toggle('show', state.selectMode && state.page === 'gallery');
  $('#selectModeBtn').textContent = state.selectMode ? '完成' : '选择';
  for (const card of $$('#grid [data-card]')) {
    const id = card.dataset.card;
    const selected = state.selected.has(id);
    const button = card.querySelector('.tile-open');
    card.classList.toggle('chosen', selected);
    card.classList.toggle('active', !state.selectMode && id === activeInspectorId);
    button.setAttribute('aria-label', (state.selectMode ? '选择 ' : '查看 ') + (items.find(it => it.id === id)?.name || ''));
    if (state.selectMode) button.setAttribute('aria-pressed', String(selected));
    else button.removeAttribute('aria-pressed');
    card.querySelectorAll('[data-copy]').forEach(b => { b.hidden = state.selectMode; });
  }
  for (const id of ['batchCopyBtn', 'batchCopyMD', 'batchDeleteBtn']) {
    $('#' + id).disabled = !state.selected.size;
  }
  if (state.selectMode) closeInspector();
}

let inspImgReq = 0;

function openInspector(id) {
  const it = items.find(item => item.id === id);
  if (!it) return;
  const wasOpen = !$('#inspector').hidden;
  activeInspectorId = id;
  if (!wasOpen) inspectorFormat = state.fmt;
  $$('#inspFmtSeg button').forEach(x => x.classList.toggle('on', x.dataset.fmt === inspectorFormat));
  $('#inspectorModal').hidden = false;
  $('#inspector').hidden = false;
  setWorkspaceModalInert(true);
  const img = $('#inspImg');
  const fullUrl = it.proxyUrl || it.url;
  const thumbUrl = it.thumbUrl || fullUrl;
  const req = ++inspImgReq;
  img.alt = it.name;
  img.classList.toggle('loading', thumbUrl !== fullUrl);
  img.src = thumbUrl;
  if (thumbUrl !== fullUrl) {
    const hi = new Image();
    hi.onload = () => {
      if (req === inspImgReq && activeInspectorId === id) {
        img.src = fullUrl;
        img.classList.remove('loading');
      }
    };
    hi.onerror = () => {
      if (req === inspImgReq) img.classList.remove('loading');
    };
    hi.src = fullUrl;
  }
  const dimText = it.width && it.height ? it.width + ' × ' + it.height : '原图尺寸';
  $('#inspName').textContent = it.name;
  $('#inspMeta').textContent = imageDate(it.ts) + ' · ' + dimText;
  $('#inspDate').textContent = imageDate(it.ts);
  $('#inspSize').textContent = fmtSize(it.sizeKB);
  $('#inspDim').textContent = dimText;
  $('#inspLinkText').textContent = formatLink(it, inspectorFormat);
  const list = galleryViewItems();
  const canStep = list.length > 1;
  $('#inspPrevBtn').disabled = !canStep;
  $('#inspNextBtn').disabled = !canStep;
  for (const card of $$('#grid [data-card]')) card.classList.toggle('active', card.dataset.card === id);
  $('#inspector').setAttribute('role', 'dialog');
  $('#inspector').setAttribute('aria-modal', 'true');
  $('#inspector').setAttribute('tabindex', '-1');
  if (!wasOpen) $('#inspector').focus({ preventScroll: true });
}

function closeInspector() {
  const wasOpen = !$('#inspector').hidden;
  const id = activeInspectorId;
  activeInspectorId = null;
  $('#inspectorModal').hidden = true;
  $('#inspector').hidden = true;
  setWorkspaceModalInert(false);
  $('#inspector').removeAttribute('role');
  $('#inspector').removeAttribute('aria-modal');
  inspImgReq++;
  setInspectorFullscreen(false);
  $('#inspImg').classList.remove('loading');
  $('#inspImg').removeAttribute('src');
  $('#inspName').textContent = '';
  $('#inspLinkText').textContent = '';
  $$('#grid .active').forEach(card => card.classList.remove('active'));
  if (wasOpen && id) {
    const card = $$('#grid [data-card]').find(item => item.dataset.card === id);
    card?.querySelector('.tile-open')?.focus({ preventScroll: true });
  }
}

function setWorkspaceModalInert(on) {
  for (const el of $$('.canvas,.sidebar,.topbar,.mobile-tabbar,#batchbar')) el.inert = on;
}

$('#inspectorModal').addEventListener('click', e => {
  if (e.target === e.currentTarget) closeInspector();
});
$('#inspCloseBtn').addEventListener('click', closeInspector);

function setInspectorFullscreen(on) {
  const stage = $('#inspStage');
  if (!stage) return;
  stage.classList.toggle('fullscreen', on);
  $('#inspPreviewBtn').setAttribute('aria-label', on ? '点击退出全屏' : '点击查看大图');
  const head = $('.insp-head');
  if (head) head.inert = on;
  for (const el of $$('.insp-body > :not(#inspStage)')) el.inert = on;
  if (on) $('#inspFullClose')?.focus({ preventScroll: true });
  else if (activeInspectorId) $('#inspPreviewBtn')?.focus({ preventScroll: true });
}

$('#inspPreviewBtn').addEventListener('click', () => {
  if (activeInspectorId) setInspectorFullscreen(!$('#inspStage').classList.contains('fullscreen'));
});
$('#inspFullClose').addEventListener('click', () => setInspectorFullscreen(false));
$('#inspStage').addEventListener('click', e => {
  if (e.target === $('#inspStage') && $('#inspStage').classList.contains('fullscreen')) setInspectorFullscreen(false);
});
$('#inspFmtSeg').addEventListener('click', e => {
  const b = e.target.closest('[data-fmt]');
  if (!b) return;
  inspectorFormat = b.dataset.fmt;
  $$('#inspFmtSeg button').forEach(x => x.classList.toggle('on', x === b));
  const it = items.find(item => item.id === activeInspectorId);
  if (it) $('#inspLinkText').textContent = formatLink(it, inspectorFormat);
});
$('#inspCopyBtn').addEventListener('click', () => {
  const it = items.find(item => item.id === activeInspectorId);
  if (it) copyText(formatLink(it, inspectorFormat));
});
$('#inspDeleteBtn').addEventListener('click', () => activeInspectorId && deleteItems([activeInspectorId]));
$('#batchAll').addEventListener('click', () => {
  const list = galleryViewItems();
  const all = list.length && list.every(it => state.selected.has(it.id));
  list.forEach(it => (all ? state.selected.delete(it.id) : state.selected.add(it.id)));
  updateGallerySelection();
});
$('#densitySeg').addEventListener('click', e => {
  const b = e.target.closest('[data-density]');
  if (!b) return;
  gridDensity = b.dataset.density;
  $$('#densitySeg button').forEach(x => x.classList.toggle('on', x === b));
  renderGallery();
});
for (const id of ['sidebarDates', 'mobileDateStrip']) {
  $('#' + id).addEventListener('click', e => {
    const b = e.target.closest('[data-date]');
    if (!b) return;
    if (state.page !== 'gallery') showPage('gallery');
    setGalleryFolder(b.dataset.date);
  });
}

let dragDepth = 0;
window.addEventListener('dragenter', e => {
  if (![...(e.dataTransfer?.types || [])].includes('Files')) return;
  e.preventDefault();
  dragDepth++;
  document.body.classList.add('file-drag');
});
window.addEventListener('dragover', e => {
  if ([...(e.dataTransfer?.types || [])].includes('Files')) e.preventDefault();
});
window.addEventListener('dragleave', () => {
  if (--dragDepth <= 0) {
    dragDepth = 0;
    document.body.classList.remove('file-drag');
  }
});
window.addEventListener('drop', e => {
  dragDepth = 0;
  document.body.classList.remove('file-drag');
  if (e.defaultPrevented || !e.dataTransfer?.files?.length) return;
  e.preventDefault();
  showPage('upload');
  addFiles(e.dataTransfer.files);
});

// Escape priority: confirm cancellation is handled by the existing listener first.
document.addEventListener('keydown', e => {
  e.workspaceOverlayWasOpen = $('#confirmOverlay').classList.contains('show') || $('#recentZoom').classList.contains('show');
}, true);
document.addEventListener('keydown', e => {
  if (e.key === 'Escape' && !$('#gallerySortMenu').hidden) {
    e.preventDefault();
    setSortMenuOpen(false);
    $('#gallerySortBtn').focus({ preventScroll: true });
    return;
  }
  if (e.key === 'Escape' && !e.workspaceOverlayWasOpen && !$('#confirmOverlay').classList.contains('show') && !$('#recentZoom').classList.contains('show')) {
    if ($('#inspStage').classList.contains('fullscreen')) setInspectorFullscreen(false);
    else closeInspector();
  }
  if (e.key === 'Tab' && !$('#inspector').hidden && !$('#confirmOverlay').classList.contains('show')) {
    const scope = $('#inspStage').classList.contains('fullscreen') ? '#inspStage button:not(:disabled)' : '#inspector button:not(:disabled)';
    const nodes = $$(scope);
    const first = nodes[0];
    const last = nodes.at(-1);
    if (nodes.length && e.shiftKey && document.activeElement === first) {
      e.preventDefault();
      last.focus();
    } else if (nodes.length && !e.shiftKey && document.activeElement === last) {
      e.preventDefault();
      first.focus();
    }
  }
});

// All Photos returns from a date view without making a second list request.
$$('[data-page="gallery"]').forEach(b => b.addEventListener('click', () => setGalleryFolder('all')));

function stepWorkspaceInspector(delta) {
  if ($('#inspector').hidden) return;
  const list = galleryViewItems();
  const index = list.findIndex(it => it.id === activeInspectorId);
  if (index >= 0 && list.length > 1) openInspector(list[(index + delta + list.length) % list.length].id);
}

$('#inspPrevBtn').addEventListener('click', () => stepWorkspaceInspector(-1));
$('#inspNextBtn').addEventListener('click', () => stepWorkspaceInspector(1));
document.addEventListener('keydown', e => {
  if ($('#confirmOverlay').classList.contains('show') || e.target.matches('input,textarea,select')) return;
  if (e.key === 'ArrowLeft') stepWorkspaceInspector(-1);
  if (e.key === 'ArrowRight') stepWorkspaceInspector(1);
});

$('#defaultFormatSeg').addEventListener('click', e => {
  const b = e.target.closest('[data-format]');
  if (b) setDefaultFormat(b.dataset.format);
});
setDefaultFormat(state.fmt);

/* ================= 初始化 ================= */
function enterPrivateUI() {
  pageInitialized = false;
  showPage(location.hash.slice(1) || 'upload');
  updateTokenUI();
  if (state.page !== 'settings') refreshStatus();
  if (state.page !== 'gallery') refreshGallery();
}

let authRevalidation = null;
function revalidateAuth() {
  if (authRevalidation) return authRevalidation;
  if (document.body.classList.contains('auth-locked')) return;
  const generation = authGeneration;
  const token = getToken();
  authRevalidation = (async () => {
    const status = await PicAPI.fetchStatus(true);
    if (generation !== authGeneration || token !== getToken() || status.unavailable) return;
    serverHasAuth = !!status.has_auth;
    if (serverHasAuth && status.authenticated !== true) {
      setToken('');
      promptAuthModal('登录已失效，请重新输入口令');
    }
  })().finally(() => {
    authRevalidation = null;
  });
  return authRevalidation;
}

async function initApp() {
  const generation = ++authGeneration;
  const token = getToken();
  const st = await PicAPI.fetchStatus(true);
  if (generation !== authGeneration || token !== getToken()) return;
  if (st.unavailable) {
    promptAuthModal('无法验证登录状态，请重试或输入口令登录');
    return;
  }
  serverHasAuth = !!st.has_auth;
  if (serverHasAuth && st.authenticated !== true) {
    if (token) setToken('');
    promptAuthModal(token ? '登录已失效，请重新输入口令' : '输入访问口令以进入图床');
    return;
  }
  setAuthLocked(false);
  enterPrivateUI();
}

window.addEventListener('storage', e => {
  if (e.key !== TOKEN_KEY && e.key !== LEGACY_TOKEN_KEY && e.key !== null) return;
  authGeneration++;
  setAuthLocked(true);
  clearPrivateUI();
  updateTokenUI();
  promptAuthModal('登录信息已变更，请重新验证');
  initApp();
});
document.addEventListener('visibilitychange', () => {
  if (!document.hidden && !authBusy) revalidateAuth();
});

initApp();
