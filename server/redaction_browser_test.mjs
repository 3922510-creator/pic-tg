// Usage: node server/redaction_browser_test.mjs CDP_HTTP OUT_DIR [LAYOUT_PROBE_PATH]
import fs from 'node:fs';
import http from 'node:http';
import assert from 'node:assert/strict';

const [cdp, out, probePath = ''] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const html = fs.readFileSync(new URL('./static/index.html', import.meta.url), 'utf8').replaceAll('{{ASSET_VERSION}}', 'test');
const css = fs.readFileSync(new URL('./static/app.css', import.meta.url));
const js = fs.readFileSync(new URL('./static/app.js', import.meta.url));
const uploads = [];

const service = http.createServer(async (req, res) => {
  const json = (data) => res.writeHead(200, { 'Content-Type': 'application/json' }).end(JSON.stringify({ code: 200, data }));
  const path = new URL(req.url, 'http://localhost').pathname;
  if (path === '/api/status') return json({ has_auth: false, authenticated: true });
  if (path === '/api/objects') return json({ list: [], total: 0 });
  if (path === '/api/upload' && req.method === 'POST') {
    const chunks = [];
    for await (const chunk of req) chunks.push(chunk);
    const form = await new Response(Buffer.concat(chunks), { headers: { 'Content-Type': req.headers['content-type'] } }).formData();
    uploads.push(form.get('file'));
    return json({ key: 'fixture.png', proxy_url: '/i/fixture.png', size: uploads.at(-1).size });
  }
  if (path === '/') return res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' }).end(html);
  if (path === '/assets/app.css') return res.writeHead(200, { 'Content-Type': 'text/css; charset=utf-8' }).end(css);
  if (path === '/assets/app.js') return res.writeHead(200, { 'Content-Type': 'application/javascript; charset=utf-8' }).end(js);
  res.writeHead(404).end();
});

const browser = await (await fetch(cdp + '/json/version')).json();
const ws = new WebSocket(browser.webSocketDebuggerUrl);
await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });
let seq = 0;
const pending = new Map();
const errors = [];
ws.onmessage = (e) => {
  const m = JSON.parse(e.data);
  const p = pending.get(m.id);
  if (p) {
    pending.delete(m.id);
    m.error ? p.reject(Error(JSON.stringify(m.error))) : p.resolve(m.result);
  }
  if (m.method === 'Runtime.exceptionThrown') errors.push(m.params.exceptionDetails.text);
};
const send = (method, params = {}, sessionId) =>
  new Promise((resolve, reject) => {
    const id = ++seq;
    pending.set(id, { resolve, reject });
    ws.send(JSON.stringify({ id, method, params, sessionId }));
  });

let collect = null;
if (probePath && fs.existsSync(probePath)) {
  const probe = fs.readFileSync(probePath, 'utf8');
  collect = new Function(probe.slice(probe.indexOf('const COLLECT ='), probe.indexOf('ws.onopen =')) + ';return COLLECT;')();
}

const { browserContextId } = await send('Target.createBrowserContext', { disposeOnDetach: true });
try {
  await new Promise((resolve) => service.listen(0, '127.0.0.1', resolve));
  const { targetId } = await send('Target.createTarget', { url: 'about:blank', browserContextId });
  const { sessionId } = await send('Target.attachToTarget', { targetId, flatten: true });
  const S = (m, p) => send(m, p, sessionId);
  const evalJS = async (expression) => {
    const r = await S('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true });
    if (r.exceptionDetails) throw Error(r.exceptionDetails.exception?.description || r.exceptionDetails.text);
    return r.result.value;
  };
  const wait = async (expr) => {
    for (let i = 0; i < 100; i++) {
      if (await evalJS(expr)) return;
      await new Promise((r) => setTimeout(r, 50));
    }
    throw Error('Timeout: ' + expr);
  };
  const painted = () => evalJS('new Promise(r=>requestAnimationFrame(()=>requestAnimationFrame(()=>r(true))))');
  const viewport = async (width, height) => {
    await S('Emulation.setDeviceMetricsOverride', { width, height, deviceScaleFactor: 1, mobile: false });
    await painted();
  };

  await S('Page.enable');
  await S('Runtime.enable');
  await viewport(1440, 950);
  await S('Page.navigate', { url: `http://127.0.0.1:${service.address().port}/` });
  await wait('typeof initApp==="function"&&!document.body.classList.contains("auth-locked")');

  await evalJS(`(async()=>{const c=document.createElement('canvas');c.width=800;c.height=480;
    const ctx=c.getContext('2d');ctx.fillStyle='#e8efff';ctx.fillRect(0,0,800,480);
    ctx.fillStyle='#203f73';ctx.font='32px sans-serif';ctx.fillText('Demo image / synthetic data',48,72);
    ctx.clearRect(740,400,50,50);
    for(let y=120;y<380;y+=4){ctx.fillStyle=y%8?'#ff6644':'#3344ee';ctx.fillRect(150,y,520,4);}
    window.original=new File([await new Promise(r=>c.toBlob(r))],'example.png',{type:'image/png'});
    const files=new DataTransfer();files.items.add(original);$('#fileInput').files=files.files;$('#fileInput').dispatchEvent(new Event('change'));})()`);
  assert.deepEqual(errors, []);
  assert(await evalJS('!!$("[data-qredact]")'));

  const open = async () => {
    await evalJS('$("[data-qredact]").click()');
    await wait('redactDialog.open&&!redactStage.hidden');
    await painted();
  };
  const drag = async (a, b, touch = false) => {
    const box = await evalJS('redactCanvas.getBoundingClientRect().toJSON()');
    const point = ([x, y]) => ({ x: Math.round(box.x + box.width * x), y: Math.round(box.y + box.height * y) });
    if (touch) {
      await S('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [point(a)] });
      await wait('redaction.drag?.mode==="pixel"');
      await S('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [point(b)] });
      await wait('!redactOutline.hidden');
      await S('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
    } else {
      await S('Input.dispatchMouseEvent', { type: 'mouseMoved', ...point(a), buttons: 0 });
      await S('Input.dispatchMouseEvent', { type: 'mousePressed', ...point(a), button: 'left', buttons: 1, clickCount: 1 });
      await wait('!!redaction.drag');
      await S('Input.dispatchMouseEvent', { type: 'mouseMoved', ...point(b), buttons: 1 });
      await wait('!redactOutline.hidden');
      await S('Input.dispatchMouseEvent', { type: 'mouseReleased', ...point(b), button: 'left', buttons: 0, clickCount: 1 });
      await wait('!redaction.drag');
    }
  };

  assert(await evalJS('uploadQuality.value==="95"&&$("#uploadQualityValue").value==="95%"&&uploadQuality.getAttribute("aria-valuetext")==="95%"'));
  await open();
  assert(await evalJS('redaction.quality===95 && $("#redactOutputHint").textContent.includes("95%")'));
  await drag([0.20, 0.28], [0.46, 0.78]);
  await evalJS('$("#uploadBtn").click()');
  assert.equal(uploads.length, 0);
  await S('Input.dispatchKeyEvent', { type: 'keyDown', key: 'Escape', code: 'Escape', windowsVirtualKeyCode: 27 });
  await wait('!redactDialog.open&&state.queue[0].file===original');

  await evalJS('uploadQuality.value="100";uploadQuality.dispatchEvent(new Event("input"))');
  assert(await evalJS('$("#uploadQualityValue").value==="原画质"'));
  await open();
  await drag([0.20, 0.28], [0.46, 0.78]);
  await evalJS('$("#redactUndo").click()');
  assert(await evalJS('$("#redactApply").disabled && redaction.regions.length===0'));
  await drag([0.20, 0.28], [0.46, 0.78]);
  await wait('redaction.regions.length===1');
  await viewport(375, 812);
  await S('Emulation.setTouchEmulationEnabled', { enabled: true, maxTouchPoints: 1 });
  await evalJS('$("#redactPixel").click()');
  await drag([0.51, 0.28], [0.80, 0.78], true);
  await wait('redaction.regions.map(r=>r.mode).join(",")==="black,pixel"');

  for (const theme of ['light', 'dark']) {
    for (const width of [320, 375, 414, 768, 1440]) {
      await viewport(width, width < 500 ? 720 : 950);
      await evalJS(`applyThemeMode('${theme}')`);
      if (collect) {
        const { defects } = await evalJS(collect(false));
        assert.deepEqual(defects.filter((d) => ['high', 'medium'].includes(d.severity)), [], `${theme} ${width}`);
      }
      if (width === 375 || width === 1440) {
        const r = await S('Page.captureScreenshot', { format: 'png' });
        fs.writeFileSync(`${out}/${theme}-${width}.png`, Buffer.from(r.data, 'base64'));
      }
    }
  }

  await evalJS(`window.realToBlob=HTMLCanvasElement.prototype.toBlob;HTMLCanvasElement.prototype.toBlob=function(cb){cb(null);};$('#redactApply').click();`);
  await wait('redactDialog.open && !$("#redactError").hidden && state.queue[0].file===original');
  assert.equal(uploads.length, 0);
  await evalJS('HTMLCanvasElement.prototype.toBlob=realToBlob;$("#redactApply").click()');
  await wait('!redactDialog.open&&state.queue[0].redacted');
  assert.equal(uploads.length, 0);

  const output = await evalJS('state.queue[0].file.arrayBuffer().then(b=>Array.from(new Uint8Array(b)))');
  assert.deepEqual(
    await evalJS(`(async()=>{const b=await createImageBitmap(new Blob([new Uint8Array(${JSON.stringify(output)})],{type:'image/png'}));const c=document.createElement('canvas');c.width=b.width;c.height=b.height;const ctx=c.getContext('2d');ctx.drawImage(b,0,0);const p=(x,y)=>Array.from(ctx.getImageData(x,y,1,1).data);return [b.width,b.height,p(220,200),p(455,200).join()===p(455,204).join(),p(20,20)];})()`),
    [800, 480, [0, 0, 0, 255], true, [232, 239, 255, 255]]
  );
  await evalJS('$("#uploadBtn").click()');
  await wait('state.queue[0].status==="done"');
  assert.equal(uploads.length, 1);
  assert.equal(uploads[0].name, 'example-打码.png');
  assert.deepEqual(Buffer.from(await uploads[0].arrayBuffer()), Buffer.from(output));

  await evalJS('addFiles([state.queue[0].file]);uploadQuality.value="80";uploadQuality.dispatchEvent(new Event("input"))');
  assert(await evalJS('!$("#uploadQualityPanel").hidden && $("#uploadQualityValue").value==="80%"'));
  for (const theme of ['light', 'dark']) {
    for (const width of [320, 375, 414, 768, 1440]) {
      await viewport(width, width < 500 ? 720 : 950);
      await evalJS(`applyThemeMode('${theme}')`);
      if (collect) {
        const { defects } = await evalJS(collect(false));
        assert.deepEqual(defects.filter((d) => ['high', 'medium'].includes(d.severity)), [], `quality ${theme} ${width}`);
      }
      if (width === 375 || width === 1440) {
        if (width === 375) {
          await evalJS('$("#uploadQualityPanel").scrollIntoView({block:"center",behavior:"instant"})');
          await painted();
        }
        const r = await S('Page.captureScreenshot', { format: 'png' });
        fs.writeFileSync(`${out}/quality-${theme}-${width}.png`, Buffer.from(r.data, 'base64'));
        if (width === 375) await evalJS('$("#drop").scrollIntoView({block:"start",behavior:"instant"})');
      }
    }
  }

  await evalJS('$("#uploadBtn").click()');
  await wait('state.queue[0].status==="done"');
  assert.equal(uploads.length, 2);
  assert.equal(uploads[1].name, 'example-打码.webp');
  assert.equal(uploads[1].type, 'image/webp');
  assert(uploads[1].size < output.length);
  const webp = Array.from(new Uint8Array(await uploads[1].arrayBuffer()));
  assert.deepEqual(
    await evalJS(`(async()=>{const b=await createImageBitmap(new Blob([new Uint8Array(${JSON.stringify(webp)})]));const c=document.createElement('canvas');c.width=b.width;c.height=b.height;const ctx=c.getContext('2d');ctx.drawImage(b,0,0);return [b.width,b.height,ctx.getImageData(760,420,1,1).data[3],Math.max(...ctx.getImageData(220,200,1,1).data.slice(0,3))<10];})()`),
    [800, 480, 0, true]
  );

  await evalJS('addFiles([original])');
  await open();
  await drag([0.20, 0.28], [0.46, 0.78]);
  assert(await evalJS('$("#redactOutputHint").textContent.includes("80%")'));
  await evalJS('$("#redactApply").click()');
  await wait('!redactDialog.open&&state.queue[0].encodedQuality===80');
  assert.equal(await evalJS('state.queue[0].file.type'), 'image/webp');
  const direct = await evalJS('state.queue[0].file.arrayBuffer().then(b=>Array.from(new Uint8Array(b)))');
  assert(direct.length < output.length);
  await evalJS('$("#uploadBtn").click()');
  await wait('state.queue[0].status==="done"');
  assert.equal(uploads.length, 3);
  assert.equal(uploads[2].name, 'example-打码.webp');
  assert.deepEqual(Buffer.from(await uploads[2].arrayBuffer()), Buffer.from(direct), 'already-compressed redaction should not be encoded again');
  assert.deepEqual(errors, []);
  console.log('PASS browser mouse/touch, direct redaction compression without double encoding, dimensions and transparency preserved');
} finally {
  service.close();
  await send('Target.disposeBrowserContext', { browserContextId });
  ws.close();
}
