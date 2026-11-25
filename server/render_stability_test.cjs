// Fast regressions; the opt-in browser test verifies actual DOM/IndexedDB.
const fs = require('node:fs');
const vm = require('node:vm');
const assert = require('node:assert/strict');

const js = fs.readFileSync(__dirname + '/static/app.js', 'utf8');

for (const name of ['showPage', 'renderGallery', 'updateGallerySelection']) {
  assert.equal([...js.matchAll(new RegExp('function ' + name + '\\(', 'g'))].length, 1, name + ' must have one implementation');
}

const navigation = js.slice(js.indexOf('function showPage(name) {'), js.indexOf('function tileRecord('));
assert(navigation.includes('if (pageInitialized && state.page === name) return;'));

const rendering = js.slice(js.indexOf('function renderGallery() {'), js.indexOf('function openInspector('));
assert(rendering.includes('syncKeyedCards(tiles, group.list.map(tileRecord));'));
assert(!rendering.includes('grid.innerHTML ='));

const selection = js.slice(js.indexOf('function toggleGalleryCard('), js.indexOf("$('#grid').addEventListener", js.indexOf('function toggleGalleryCard(')));
assert(!selection.includes('renderGallery()'));

const foreground = js.slice(js.indexOf('let authRevalidation = null;'), js.indexOf('async function initApp() {'));
(async () => {
  let token = 'valid';
  let locked = false;
  let prompted = 0;
  let resolveStatus;
  let statusCalls = 0;
  const c = vm.createContext({
    authGeneration: 12,
    serverHasAuth: true,
    getToken: () => token,
    document: { body: { classList: { contains: () => locked } } },
    PicAPI: {
      fetchStatus: async () => {
        statusCalls++;
        return { has_auth: true, authenticated: true };
      },
    },
    setToken: (value) => {
      token = value;
      locked = true;
    },
    promptAuthModal: () => { prompted++; },
  });
  vm.runInContext(foreground, c);

  await c.revalidateAuth();
  assert.equal(statusCalls, 1);
  assert.equal(c.authGeneration, 12);
  assert.equal(token, 'valid');
  assert.equal(prompted, 0);

  c.PicAPI.fetchStatus = () => {
    statusCalls++;
    return new Promise((resolve) => { resolveStatus = resolve; });
  };
  const first = c.revalidateAuth();
  const second = c.revalidateAuth();
  assert.equal(first, second);
  assert.equal(statusCalls, 2);
  resolveStatus({ unavailable: true });
  await first;
  assert.equal(locked, false);

  const stale = c.revalidateAuth();
  token = 'new-session';
  c.authGeneration++;
  resolveStatus({ has_auth: true, authenticated: false });
  await stale;
  assert.equal(token, 'new-session');
  assert.equal(prompted, 0);

  c.PicAPI.fetchStatus = async () => ({ has_auth: true, authenticated: false });
  await c.revalidateAuth();
  assert.equal(token, '');
  assert.equal(locked, true);
  assert.equal(prompted, 1);

  await c.revalidateAuth();
  assert.equal(prompted, 1);

  console.log('PASS single navigation/render implementation with same-page guard; keyed renderer; selection does not rerender; foreground auth deduped, preserves generation, handles offline/stale/invalid responses safely');
})().catch((error) => {
  console.error(error);
  process.exitCode = 1;
});
