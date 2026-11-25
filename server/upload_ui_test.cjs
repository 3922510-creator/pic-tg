// Run with: node server/upload_ui_test.cjs
const fs = require('node:fs');
const vm = require('node:vm');
const assert = require('node:assert/strict');

const html = fs.readFileSync(__dirname + '/static/index.html', 'utf8');
const css = fs.readFileSync(__dirname + '/static/app.css', 'utf8');
const js = fs.readFileSync(__dirname + '/static/app.js', 'utf8');

// Verify syntax of external script and inline theme script.
new vm.Script(js);
for (const [, script] of html.matchAll(/<script>([\s\S]*?)<\/script>/g)) new vm.Script(script);

// Ensure API/PicGo examples are populated dynamically rather than hardcoding a deployment domain.
assert(!html.includes('/api/upload" -H "Authorization: Bearer YOUR_API_KEY"'));

const sizeContext = vm.createContext({});
vm.runInContext(js.slice(js.indexOf('function fmtSize('), js.indexOf('let toastTimer = null;')), sizeContext);
assert.equal(sizeContext.fmtSize(394 / 1024), '394 B');
assert(!html.includes('本地压缩'));
assert(!html.includes('PNG / GIF 原样上传'));
assert(html.indexOf('id="queue"') < html.indexOf('id="uploadBtn"'));
assert(!html.includes('服务端压缩 · 保留原尺寸'));
assert(css.includes('overflow-wrap:anywhere'));

// Every literal DOM lookup in app.js must exist in index.html.
const domIDs = new Set([...html.matchAll(/\bid="([^"]+)"/g)].map((m) => m[1]));
for (const [, id] of js.matchAll(/\$\('#([A-Za-z0-9_-]+)'\)/g)) {
  assert(domIDs.has(id), 'missing DOM target #' + id);
}

const sliceBetween = (src, start, end) => {
  const a = src.indexOf(start);
  const b = src.indexOf(end, a + start.length);
  assert(a >= 0 && b > a, `slice failed: ${start} .. ${end}`);
  return src.slice(a, b);
};
const authHelper = sliceBetween(js, 'function authHeaders(', 'async function authFetch(');
const adapter = sliceBetween(js, 'const PicAPI =', '/* ================= 状态');
const preparation =
  sliceBetween(js, 'const uploadMaxBytes =', '/* ================= PicAPI') +
  sliceBetween(js, 'function editableRaster(', 'function redactStatus(');

let mode = 'success';
let lastXHR;
let token = '';
let fetchImpl;
let lastForm;
let lastFetchInit;

class XHR {
  constructor() {
    this.upload = {};
    this.headers = {};
    lastXHR = this;
  }
  open() {}
  setRequestHeader(k, v) { this.headers[k] = v; }
  send() {
    queueMicrotask(() => {
      this.upload.onprogress({ lengthComputable: true, loaded: 100, total: 100 });
      this.upload.onload();
      if (mode === 'timeout') return this.ontimeout();
      if (mode === 'abort') return this.onabort();
      if (mode === 'network') return this.onerror();
      this.status = mode === 'proxy' ? 502 : 200;
      this.responseText = mode === 'proxy'
        ? '<html>Bad Gateway</html>'
        : JSON.stringify({
            code: 200,
            data: {
              key: 'test.png',
              size: 100,
              proxy_url: 'https://pic.test/images/test.png',
              direct_url: 'https://pic.test/direct/test.png',
            },
          });
      this.onload();
    });
  }
}

const context = vm.createContext({
  serverHasAuth: false,
  getToken: () => token,
  setToken: (t) => { token = t; },
  FormData: class {
    constructor() {
      this.fields = new Map();
      lastForm = this;
    }
    append(k, v) { this.fields.set(k, v); }
  },
  XMLHttpRequest: XHR,
  Error,
  console,
  authFetch: (url, init) => {
    lastFetchInit = init;
    return fetchImpl(url, init);
  },
  fetch: (url, init) => {
    lastFetchInit = init;
    return fetchImpl(url, init);
  },
});
vm.runInContext(authHelper + preparation + adapter + ';globalThis.api=PicAPI;globalThis.prepare=prepareImage;globalThis.hdrs=authHeaders;', context);

// Verify authHeaders omits empty Authorization in passwordless mode.
assert.deepEqual({ ...context.hdrs({ 'X-Test': '1' }) }, { 'X-Test': '1' });
token = 'secret-tok';
assert.deepEqual({ ...context.hdrs({ 'X-Test': '1' }) }, { 'X-Test': '1', Authorization: 'Bearer secret-tok' });
token = '';

const reply = (data, code = 200) => ({
  ok: code === 200,
  status: code,
  json: async () => ({ code, data, msg: 'expected failure' }),
});

(async () => {
  for (const type of ['image/png', 'image/gif']) {
    const original = { size: 100, type };
    assert.equal(await context.prepare(original), original);
  }
  const phases = [];
  const result = await context.api.upload({ size: 100, type: 'image/png' }, (p, s) => phases.push([p, s]));
  assert.equal(result.key, 'test.png');
  assert.equal(lastXHR.timeout, 270000);
  assert.equal(result.url, 'https://pic.test/images/test.png');
  assert.equal(lastForm.fields.has('token'), false, 'upload should not send redundant token form field');
  assert.equal(lastXHR.headers.Authorization, undefined, 'passwordless upload should not send empty Authorization header');

  await assert.rejects(context.api.upload({ size: 20 * 1024 * 1024 + 1, type: 'image/png' }, () => {}), /20 MB/);
  assert(js.includes('url: q.result.proxyUrl || q.result.url'));
  assert(phases.some(([p, s]) => p === 98 && s.includes('存入图床')));

  for (const [scenario, message] of [['timeout', '超时'], ['abort', '取消'], ['network', '连接中断'], ['proxy', 'HTTP 502']]) {
    mode = scenario;
    await assert.rejects(context.api.upload({ size: 100, type: 'image/png' }, () => {}), (e) => e.message.includes(message));
  }

  let calls = 0;
  fetchImpl = async () => reply({
    total: 1001,
    list: Array.from({ length: ++calls === 1 ? 1000 : 1 }, (_, i) => ({
      key: `test${calls}-${i}.png`,
      size: 100,
      proxy_url: 'https://pic.test/images/test.png',
      direct_url: 'https://pic.test/direct/test.png',
      created_at: '2026-11-02 04:00:00',
    })),
  });
  const list = await context.api.list();
  assert.equal(list.length, 1001);
  assert.equal(calls, 2);
  assert.equal(list[0].ts, Date.parse('2026-11-02T04:00:00Z'));
  assert.equal(list[0].url, 'https://pic.test/images/test.png');
  assert.equal(lastFetchInit.headers.Authorization, undefined);

  fetchImpl = async () => reply({}, 500);
  await assert.rejects(context.api.list(), /expected failure/);
  await assert.rejects(context.api.batchRemove(['a']), /expected failure/);

  fetchImpl = async () => reply({ deleted_keys: ['a'], errors: ['b failed'] });
  const partial = await context.api.batchRemove(['a', 'b']);
  assert.equal(partial.deleted_keys.length, 1);
  assert.equal(partial.errors.length, 1);

  fetchImpl = async () => { throw Error('offline'); };
  context.serverHasAuth = true;
  const offline = await context.api.fetchStatus();
  assert.equal(offline.has_auth, true);
  assert.equal(offline.unavailable, true);

  assert(js.includes('} finally {\n    uploading = false;'));
  assert(!js.includes('settings.target'));

  for (const [type, name] of [['image/jpeg', 'photo.jpeg'], ['image/webp', 'a.webp'], ['image/png', 'a.png'], ['image/gif', 'a.gif'], ['', 'a.JPG']]) {
    const original = { size: 1000, type, name };
    assert.equal(await context.prepare(original), original);
  }

  let encodedSize;
  let encode;
  let closed = 0;
  context.window = { createImageBitmap: true };
  context.createImageBitmap = async () => ({ width: 4032, height: 3024, close() { closed++; } });
  context.document = {
    createElement: () => ({
      getContext: () => ({ drawImage() {} }),
      toBlob(callback, mime, quality) {
        encodedSize = [this.width, this.height];
        encode = [mime, quality];
        callback({ size: 500, type: mime });
      },
    }),
  };
  context.File = class {
    constructor(parts, name, options) {
      this.name = name;
      this.type = options.type;
      this.size = parts.reduce((n, p) => n + p.size, 0);
    }
  };
  const converted = await context.prepare({ size: 1000, type: 'image/avif', name: 'photo.avif' });
  assert.equal(converted.name, 'photo.png');
  assert.deepEqual(encodedSize, [4032, 3024]);
  assert.deepEqual(encode, ['image/png', undefined]);
  assert.equal(closed, 1);

  context.serverHasAuth = false;
  mode = 'success';
  const jpg = { size: 1000, type: 'image/jpeg', name: 'photo.jpeg' };
  await context.api.upload(jpg, () => {});
  assert.equal(lastForm.fields.get('file'), jpg);
  assert.equal(lastForm.fields.has('strip_exif'), false);
  assert.equal(lastForm.fields.get('orig_size'), '1000');

  const header = new Uint8Array(30);
  header.set([0xff, 0xd8]);
  const largeJpg = { ...jpg, size: 22 * 1024 * 1024, slice: () => new Blob([header]) };
  await context.api.upload(largeJpg, () => {}, 75);
  assert.deepEqual(encode, ['image/jpeg', 0.75]);
  assert.equal(lastForm.fields.get('file').size, 500);
  assert.equal(lastForm.fields.get('orig_size'), String(largeJpg.size));

  const gif = { type: 'image/gif', name: 'anim.gif', size: 512 };
  await context.api.upload(gif, () => {}, 65);
  assert.equal(lastForm.fields.get('file'), gif);

  const apng = Buffer.from('89504e470d0a1a0a000000086163544c000000020000000000000000000000004944415400000000', 'hex');
  const animatedPng = new Blob([apng], { type: 'image/png' });
  assert.equal(await context.prepare(animatedPng, 60), animatedPng);

  console.log('PASS: original/opt-in quality, animated bypass, large JPG compressed below limit, bitmap cleanup, upload recovery, list/auth, passwordless headers');
})().catch((e) => {
  console.error(e);
  process.exitCode = 1;
});

// Date folders group by local browser timezone without empty days.
const folders = sliceBetween(js, 'function imageDate(', 'function setGalleryFolder(');
const folderContext = vm.createContext({ Intl, Date, Map });
vm.runInContext(folders, folderContext);
const t1 = Date.UTC(2026, 10, 2, 12, 0, 0);
const t2 = Date.UTC(2026, 10, 4, 12, 0, 0);
const t3 = Date.UTC(2026, 10, 4, 13, 0, 0);
const grouped = folderContext.dateFolders([{ ts: t1, sizeKB: 10 }, { ts: t2, sizeKB: 20 }, { ts: t3, sizeKB: 30 }]);
assert.equal(grouped.length, 2);
assert.equal(grouped[0].count, 2);
assert.equal(grouped[0].sizeKB, 50);
assert.equal(folderContext.dateFolders([]).length, 0);

const markdown = sliceBetween(js, 'function markdownImage(', 'function closeRecentZoom(');
vm.runInContext(markdown, folderContext);
assert.equal(folderContext.markdownImage({ name: 'a[b].jpg', proxyUrl: 'https://pic.test/images/a.jpg' }), '![a\\[b\\].jpg](https://pic.test/images/a.jpg)');
assert(html.includes('<dialog class="dialog telegram-dialog"'));
assert(!html.includes('closeSmsModal'));
console.log('PASS: date folders have no empty days; local timezone; Markdown escaping; native modal');

// Local history must not import the server gallery or copy login credentials.
const recentContext = vm.createContext({ items: [{ id: 'server-only', url: 'https://pic.test/server', ts: 99 }], localRecent: [], state: { fmt: 'url' } });
const formatOnly = sliceBetween(js, 'function formatLink(', '/* ================= Browser-local');
const recentFunctions = sliceBetween(js, 'function recentUploads(', 'function renderRecent(');
vm.runInContext(formatOnly + recentFunctions, recentContext);
assert.equal(recentContext.recentLinks(), '');
recentContext.localRecent = [
  { id: 'local', name: 'my-photo.png', url: 'https://pic.test/local', ts: 1 },
  { id: 'another', name: 'second.png', url: 'https://pic.test/second', ts: 2 },
];
assert.equal(recentContext.recentLinks(), 'https://pic.test/local\nhttps://pic.test/second');
assert(js.includes('indexedDB.open(RECENT_DB, 1)'));
assert(js.includes('thumbnail instanceof Blob'));
const galleryRefresh = sliceBetween(js, 'async function refreshGallery(', 'function filteredItems(');
assert(!galleryRefresh.includes('loadLocalRecent'));
assert(!galleryRefresh.includes('renderRecent'));
assert(!js.includes('copyText(getToken()'));
assert(!js.includes('PicAPI.changePassword(oldTok, newTok)'));
console.log('PASS: recent links use browser-local records only; gallery cannot backfill them; independent API key UI');

const batchHandler = sliceBetween(js, "$('#batchCopyMD').addEventListener", "$('#batchDeleteBtn').addEventListener");
let batchClick;
let copiedBatch = '';
let batchMessage = '';
const batchContext = vm.createContext({
  $: () => ({ addEventListener: (_, fn) => { batchClick = fn; } }),
  serverHasAuth: false,
  getToken: () => '',
  state: { selected: new Set(['a', 'b']) },
  items: [
    { id: 'a', name: 'a.png', proxyUrl: 'https://pic.test/a' },
    { id: 'b', name: 'b.png', proxyUrl: 'https://pic.test/b' },
    { id: 'c', name: 'c.png', proxyUrl: 'https://pic.test/c' },
  ],
  copyText: (text) => { copiedBatch = text; },
  toast: (text) => { batchMessage = text; },
});
vm.runInContext(markdown + batchHandler, batchContext);
batchClick();
assert.equal(copiedBatch, '![a.png](https://pic.test/a)\n![b.png](https://pic.test/b)');
batchContext.state.selected.clear();
batchClick();
assert.equal(batchMessage, '先勾选图片');
console.log('PASS: actual batch Markdown handler copies selected only, one per line; empty selection is guarded');

const cardToggle = js.slice(js.indexOf('function toggleGalleryCard('), js.indexOf("$('#grid').addEventListener", js.indexOf('function toggleGalleryCard(')));
const cardContext = vm.createContext({
  state: { selectMode: true, selected: new Set() },
  updateGallerySelection() {},
  $: () => ({ querySelectorAll: () => [] }),
});
vm.runInContext(cardToggle, cardContext);
cardContext.toggleGalleryCard('a');
assert(cardContext.state.selected.has('a'));
cardContext.toggleGalleryCard('a');
assert.equal(cardContext.state.selected.size, 0);
cardContext.state.selectMode = false;
cardContext.toggleGalleryCard('a');
assert.equal(cardContext.state.selected.size, 0);
assert(!html.includes('class="pick"'));
assert(!html.includes('data-pick='));
console.log('PASS: card toggle selects/deselects only in batch mode; checkbox removed');
