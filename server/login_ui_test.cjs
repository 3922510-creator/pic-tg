// Run: node server/login_ui_test.cjs
const fs = require('node:fs');
const vm = require('node:vm');
const assert = require('node:assert/strict');

const html = fs.readFileSync(__dirname + '/static/index.html', 'utf8');
const css = fs.readFileSync(__dirname + '/static/app.css', 'utf8');
const js = fs.readFileSync(__dirname + '/static/app.js', 'utf8');

assert(html.includes('<body class="auth-locked">'));
assert(!html.includes('authCancelBtn'));
assert(css.includes('body.auth-locked > :not(#authOverlay):not(#toast):not(script){display:none!important}'));

const login = html.slice(html.indexOf('<!-- 独立全屏登录页'), html.indexOf('<!-- 修改密码弹窗'));
assert(login.includes('type="submit"'));
assert(!login.includes('取消'));

const bootstrap = js.slice(js.indexOf('async function initApp() {'), js.indexOf("window.addEventListener('storage'"));
(async () => {
  for (const [status, stored, expectUnlock] of [
    [{ has_auth: true, authenticated: false }, '', false],
    [{ has_auth: true, authenticated: false }, 'expired', false],
    [{ has_auth: true, authenticated: true }, 'valid', true],
    [{ has_auth: false, authenticated: true }, '', true],
    [{ unavailable: true }, 'valid', false],
  ]) {
    let token = stored;
    let unlocked = false;
    let prompted = false;
    const c = vm.createContext({
      authGeneration: 0,
      serverHasAuth: true,
      getToken: () => token,
      PicAPI: {
        fetchStatus: async (only) => {
          assert.equal(only, true);
          return status;
        },
      },
      setToken: (t) => { token = t; },
      promptAuthModal: () => { prompted = true; },
      setAuthLocked: (locked) => { unlocked = !locked; },
      location: { hash: '#gallery' },
      enterPrivateUI() {},
      showPage() {},
      updateTokenUI() {},
      refreshStatus() {},
      refreshGallery() {},
    });
    vm.runInContext(bootstrap, c);
    await c.initApp();
    assert.equal(unlocked, expectUnlock);
    assert.equal(prompted, !expectUnlock);
    if (stored === 'expired') assert.equal(token, '');
  }

  // A delayed initialization response may not overwrite a newer login/token.
  let resolve;
  const c = vm.createContext({
    authGeneration: 0,
    getToken: () => '',
    PicAPI: { fetchStatus: () => new Promise((r) => { resolve = r; }) },
    setAuthLocked: () => assert.fail('stale auth response unlocked UI'),
  });
  vm.runInContext(bootstrap, c);
  const pending = c.initApp();
  c.authGeneration++;
  resolve({ has_auth: false, authenticated: true });
  await pending;

  const close = js.slice(js.indexOf('function closeAuthModal() {'), js.indexOf("$('#authForm').addEventListener"));
  const guard = vm.createContext({ setAuthLocked: () => assert.fail('Escape unlocked login') });
  vm.runInContext(close, guard);
  guard.closeAuthModal();

  console.log('PASS: default opaque login gate; no cancel; anonymous/invalid/offline locked; valid/public unlocked; stale response ignored; Escape cannot unlock');
})().catch((e) => {
  console.error(e);
  process.exitCode = 1;
});
