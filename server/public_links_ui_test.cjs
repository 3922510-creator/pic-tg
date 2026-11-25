const fs = require('node:fs');
const vm = require('node:vm');
const assert = require('node:assert/strict');

const js = fs.readFileSync(__dirname + '/static/app.js', 'utf8');
const source = js.slice(js.indexOf('async function upgradeRecentLinks('), js.indexOf('async function loadLocalRecent('));
const key = 'telegram/0123456789abcdef/2026/11/02/abcdef123456.png';
const url = 'https://pic.example/i/0123456789abcdef0123456789abcdef.png';

(async () => {
  const db = new Map();
  let writes = 0;
  let requests = 0;
  const ctx = vm.createContext({
    URL,
    location: { origin: 'https://pic.example' },
    recentGeneration: 1,
    getToken: () => 'secret',
    authHeaders: (extra) => ({ ...(extra || {}), Authorization: 'Bearer secret' }),
    fetch: async () => {
      requests++;
      return { ok: true, json: async () => ({ code: 200, data: { [key]: url } }) };
    },
    recentTransaction: async (mode, run) => {
      run({
        get(k) {
          const req = {};
          queueMicrotask(() => {
            req.result = db.get(k);
            req.onsuccess();
          });
          return req;
        },
        put(row) {
          writes++;
          db.set(row.key, { ...row });
        },
      });
      await new Promise((resolve) => setImmediate(resolve));
    },
  });
  vm.runInContext(source, ctx);

  const row = { key, ts: 12, url: 'https://pic.example/images/' + key, thumbnail: 'unchanged' };
  db.set(key, { ...row });
  await ctx.upgradeRecentLinks([row], 1, 'secret');
  assert.equal(row.url, url);
  assert.equal(db.get(key).proxyUrl, url);
  assert.equal(db.get(key).thumbnail, 'unchanged');
  assert.equal(writes, 1);

  await ctx.upgradeRecentLinks([row], 1, 'secret');
  assert.equal(requests, 1);

  // A network reply after removal must not put the removed record back.
  db.clear();
  await ctx.upgradeRecentLinks([{ ...row, url: 'https://pic.example/images/' + key, proxyUrl: '' }], 1, 'secret');
  assert.equal(db.size, 0);
  assert.equal(writes, 1);

  // A clear/logout changes the generation while the request is pending.
  db.set(key, { ...row });
  ctx.fetch = async () => {
    ctx.recentGeneration++;
    return { ok: true, json: async () => ({ code: 200, data: { [key]: url } }) };
  };
  await ctx.upgradeRecentLinks([{ ...row, url: 'https://pic.example/images/' + key, proxyUrl: '' }], 1, 'secret');
  assert.equal(writes, 1);

  console.log('PASS: old local links migrate and persist; thumbnail untouched; no repeat request; removal/clear/logout cannot resurrect history');
})().catch((error) => {
  console.error(error);
  process.exitCode = 1;
});
