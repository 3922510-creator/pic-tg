const fs = require('node:fs');
const vm = require('node:vm');
const assert = require('node:assert/strict');

const js = fs.readFileSync(__dirname + '/static/app.js', 'utf8');
const source = js.slice(js.indexOf('async function runUploadPool('), js.indexOf('let uploading = false;'));
const c = vm.createContext({ Promise, Array, Math });
vm.runInContext(source, c);

(async () => {
  let active = 0;
  let max = 0;
  const started = [];
  const finished = [];
  await c.runUploadPool([0, 1, 2, 3, 4, 5], 3, async (n) => {
    started.push(n);
    active++;
    max = Math.max(max, active);
    await new Promise((r) => setTimeout(r, 15 + n));
    active--;
    finished.push(n);
  });
  assert.equal(max, 3);
  assert.equal(finished.length, 6);
  assert.equal(new Set(started).size, 6);

  let token = 'old';
  let calls = 0;
  await c.runUploadPool([0, 1, 2, 3, 4, 5], 3, async () => {
    if (token !== 'old') return;
    calls++;
    await new Promise((r) => setTimeout(r, 5));
    token = 'new';
  });
  assert.equal(calls, 3);

  const add = js.slice(js.indexOf('function addFiles('), js.indexOf('function renderQueue('));
  const queue = [];
  let message = '';
  let urls = 0;
  const a = vm.createContext({
    serverHasAuth: false,
    uploading: false,
    redaction: null,
    getToken: () => '',
    state: { get queue() { return queue; } },
    seedN: 0,
    URL: { createObjectURL: () => { urls++; return 'blob:test'; } },
    toast: (m) => { message = m; },
    renderQueue() {},
  });
  vm.runInContext(add, a);

  const image = { type: 'image/png', name: 'x.png', size: 100 };
  a.addFiles(Array(7).fill(image));
  assert.equal(queue.length, 0);
  assert.equal(urls, 0);
  assert(message.includes('6'));

  a.addFiles(Array(6).fill(image));
  assert.equal(queue.length, 6);
  a.addFiles([image]);
  assert.equal(queue.length, 6);

  queue.forEach((q) => { q.status = 'done'; });
  a.addFiles([image]);
  assert.equal(queue.filter((q) => q.status === 'waiting').length, 1);

  a.uploading = true;
  a.addFiles([image]);
  assert(message.includes('上传中'));
  assert(js.includes('void rememberLocalUpload(item, q.file, historyVersion);'));

  console.log('PASS actual worker pool: max3 concurrent, six exactly once, token-change stops pending; batch cap rejects whole extra selection, completed records do not consume capacity; local cache nonblocking');
})().catch((e) => {
  console.error(e);
  process.exitCode = 1;
});
