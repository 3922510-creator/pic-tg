// Usage: node server/workspace_browser_test.mjs BASE_URL CONFIG_PATH CDP_HTTP OUT_DIR [LAYOUT_PROBE_PATH]
// Read-only service QA. Upload fixtures run against a browser-local mock only.
import fs from 'node:fs';
import assert from 'node:assert/strict';

const [base, configPath, cdp, out, probePath = ''] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true, mode: 0o700 });
const cfg = JSON.parse(fs.readFileSync(configPath));
const browser = await (await fetch(cdp + '/json/version')).json();
const ws = new WebSocket(browser.webSocketDebuggerUrl);
await new Promise((r) => { ws.onopen = r; });

let next = 0;
const pending = new Map();
const errors = [];
const requests = [];
const writes = [];
ws.onmessage = (e) => {
  const m = JSON.parse(e.data);
  if (m.id) {
    const p = pending.get(m.id);
    if (p) {
      pending.delete(m.id);
      m.error ? p.reject(Error(JSON.stringify(m.error))) : p.resolve(m.result);
    }
  } else if (m.method === 'Runtime.exceptionThrown') {
    errors.push(m.params.exceptionDetails.exception?.description || m.params.exceptionDetails.text);
  } else if (m.method === 'Network.requestWillBeSent') {
    requests.push(m.params.request.url);
    if (m.params.request.method !== 'GET' && !m.params.request.url.includes('/api/auth/verify')) {
      writes.push(m.params.request.method + ' ' + m.params.request.url);
    }
  }
};
const send = (method, params = {}, sessionId) =>
  new Promise((resolve, reject) => {
    const id = ++next;
    pending.set(id, { resolve, reject });
    ws.send(JSON.stringify({ id, method, params, sessionId }));
  });

const { browserContextId } = await send('Target.createBrowserContext', { disposeOnDetach: true });
try {
  const { targetId } = await send('Target.createTarget', { url: 'about:blank', browserContextId });
  const { sessionId } = await send('Target.attachToTarget', { targetId, flatten: true });
  const S = (m, p) => send(m, p, sessionId);
  const ev = async (expression) => {
    const r = await S('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true });
    if (r.exceptionDetails) throw Error(r.exceptionDetails.exception?.description || r.exceptionDetails.text);
    return r.result.value;
  };
  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
  const wait = async (expression) => {
    for (let i = 0; i < 200; i++) {
      if (await ev(expression)) return;
      await sleep(100);
    }
    throw Error('Timeout ' + expression);
  };

  await S('Page.enable');
  await S('Runtime.enable');
  await S('Network.enable');
  await S('Emulation.setDeviceMetricsOverride', { width: 1440, height: 1000, deviceScaleFactor: 1, mobile: false });
  await S('Page.navigate', { url: base });
  await wait('typeof initApp==="function"');
  await wait('document.querySelector("#authMsg").textContent.includes("输入")');
  assert(await ev('document.body.classList.contains("auth-locked")&&getComputedStyle(document.querySelector("#appShell")).display==="none"'));
  await S('Input.dispatchKeyEvent', { type: 'keyDown', key: 'Escape', code: 'Escape', windowsVirtualKeyCode: 27 });
  assert(await ev('document.body.classList.contains("auth-locked")'));
  console.log('PASS anonymous opaque lock; Escape cannot bypass');

  // Populate only a fresh test browser, do not print or screenshot credentials.
  await ev(`document.querySelector('#authInput').value=${JSON.stringify(cfg.auth_token)};document.querySelector('#authForm').requestSubmit();`);
  await wait('!document.body.classList.contains("auth-locked")&&items.length>0');
  await sleep(400);
  assert.equal(requests.filter((u) => u.includes('/api/objects?')).length, 1);
  assert.equal(await ev('state.page'), 'upload');
  assert.deepEqual(await ev('[...document.querySelectorAll(".mobile-tabbar [data-page]")].map(b=>b.dataset.page)'), ['upload', 'gallery', 'api', 'settings']);
  assert.deepEqual(await ev('[...document.querySelectorAll(".sidebar [data-page]")].map(b=>b.dataset.page)'), ['upload', 'gallery', 'api', 'settings']);

  await ev('showPage("gallery")');
  await sleep(300);
  assert(await ev('!document.querySelector("#sbUploadBtn")&&!document.querySelector("#tbUploadBtn")&&!document.querySelector("#uploadRibbon")'));
  await wait('[...document.querySelectorAll("#grid img")].some(img=>img.naturalWidth>0)');
  assert.equal(await ev('localRecent.length'), 0);

  await ev('window.cards=[...document.querySelectorAll("#grid [data-card]")];window.images=[...document.querySelectorAll("#grid img")];window.mutations=0;window.observer=new MutationObserver(rs=>mutations+=rs.filter(r=>r.type==="childList").length);observer.observe(document.querySelector("#grid"),{childList:true,subtree:true});document.querySelector("#selectModeBtn").click();document.querySelector("#grid .tile-open").click();');
  assert.equal(await ev('state.selected.size'), 1);
  await ev('document.querySelector("#batchAll").click()');
  assert.equal(await ev('state.selected.size'), await ev('items.length'));
  await ev('window.copied="";navigator.clipboard.writeText=async t=>{copied=t;};document.querySelector("#batchCopyMD").click()');
  await wait('copied.startsWith("![")');
  await ev('document.querySelector("#selectModeBtn").click();refreshGallery()');
  await sleep(500);
  assert(await ev('cards.every((n,i)=>document.querySelectorAll("#grid [data-card]")[i]===n)&&images.every((n,i)=>document.querySelectorAll("#grid img")[i]===n)'));
  assert.equal(await ev('mutations'), 0);
  await ev('window.generation=authGeneration;revalidateAuth()');
  assert.equal(await ev('authGeneration'), await ev('generation'));
  console.log('PASS actual new DOM: selection/select-all/MD, stable card and image nodes, one initial list, foreground auth');

  await ev('document.querySelector("#grid .tile-open").click()');
  assert(await ev('!document.querySelector("#inspector").hidden'));
  await ev('document.querySelector("#inspFmtSeg [data-fmt=md]").click();document.querySelector("#inspCopyBtn").click()');
  await wait('copied.startsWith("![")');
  await ev('document.querySelector("#inspPreviewBtn").click()');
  assert(await ev('document.querySelector("#inspStage").classList.contains("fullscreen")'));
  await ev('document.querySelector("#inspFullClose").click()');
  assert(!await ev('document.querySelector("#inspStage").classList.contains("fullscreen")'));
  await ev('document.querySelector("#inspDeleteBtn").click();true');
  await wait('document.querySelector("#confirmOverlay").classList.contains("show")');
  assert(await ev('(()=>{const b=document.querySelector("#confirmCancel"),r=b.getBoundingClientRect();return document.elementFromPoint(r.x+r.width/2,r.y+r.height/2)===b;})()'));
  await ev('closeConfirm(false);closeInspector()');

  await ev('document.querySelector("#gallerySortBtn").click();document.querySelector("#gallerySortMenu [data-sort=old]").click()');
  assert(await ev('state.sort==="old"&&document.querySelector("#gallerySortMenu").hidden&&document.querySelector("#gallerySortLabel").textContent==="最早优先"&&Array.from(document.querySelectorAll("#grid .date-group-head h2")).map(n=>n.textContent).join()===dateFolders(items).reverse().map(g=>g.date).join()'));
  await ev('document.querySelector("#gallerySortBtn").click();document.querySelector("#gallerySortMenu [data-sort=new]").click();document.querySelector("#gallerySearch").value="impossible-no-result";renderGallery()');
  assert(await ev('!document.querySelector("#galleryEmpty").hidden'));
  await ev('document.querySelector("#gallerySearch").value="";renderGallery();document.querySelector("#densitySeg [data-density=compact]").click()');
  assert(await ev('document.querySelector(".photo-grid").classList.contains("compact")'));
  await ev('document.querySelector("#densitySeg [data-density=cozy]").click()');
  await ev('document.querySelector("#gallerySortBtn").click()');
  assert(await ev('!document.querySelector("#gallerySortMenu").hidden&&document.querySelector("#gallerySortBtn").getAttribute("aria-expanded")==="true"'));
  await S('Input.dispatchKeyEvent', { type: 'keyDown', key: 'Escape', code: 'Escape', windowsVirtualKeyCode: 27 });
  assert(await ev('document.querySelector("#gallerySortMenu").hidden&&document.querySelector("#gallerySortBtn").getAttribute("aria-expanded")==="false"'));
  console.log('PASS inspector formats, delete confirmation cancellation/hit-test, old-date sorting, empty search and density');

  // Verify API page uses dynamic location.origin and PicGo copy button works.
  await ev('showPage("api")');
  await sleep(150);
  assert(await ev('document.querySelector("#apiUploadEndpoint").textContent.includes(location.origin + "/api/upload")'));
  assert(await ev('document.querySelector("#apiCurlCode").textContent.includes(location.origin + "/api/upload")'));
  assert(await ev('document.querySelector("#picgoCode").textContent.includes(location.origin + "/api/upload")'));
  await ev('window.copied="";document.querySelector("#picgoCopyBtn").click()');
  await wait('copied.includes("/api/upload")');
  console.log('PASS API page dynamic origin and PicGo copy button');

  // Browser-only uploads exercise the production queue/persistence without writing to Telegram.
  await ev(`(async()=>{showPage('upload');const canvas=document.createElement('canvas');canvas.width=8;canvas.height=8;window.fixtureBlob=await new Promise(r=>canvas.toBlob(r,'image/png'));window.fixtureFile=new File([fixtureBlob],'本机[测试].png',{type:'image/png'});addFiles(Array(7).fill(fixtureFile));})()`);
  assert.equal(await ev('state.queue.length'), 0);
  await ev(`window.realUpload=PicAPI.upload;window.activeUploads=0;window.peakUploads=0;window.uploadSerial=0;PicAPI.upload=async(file,progress)=>{activeUploads++;peakUploads=Math.max(peakUploads,activeUploads);const n=++uploadSerial;progress(98,'正在存入图床');await new Promise(r=>setTimeout(r,60));activeUploads--;return {key:'mock-local-'+n+'.png',url:location.origin+'/i/mock-local-'+n+'.png',proxyUrl:location.origin+'/i/mock-local-'+n+'.png',sizeKB:1,origKB:1};};addFiles(Array(6).fill(fixtureFile));document.querySelector('#uploadBtn').click();`);
  await wait('!uploading&&state.queue.length===6&&state.queue.every(q=>q.status==="done")');
  await wait('localRecent.length===6');
  assert.equal(await ev('peakUploads'), 3);
  assert(await ev('localRecent.every(it=>it.thumbnail instanceof Blob&&it.name==="本机[测试].png")'));
  await ev('PicAPI.upload=realUpload;refreshGallery();loadLocalRecent()');
  await sleep(300);
  await S('Page.reload', { ignoreCache: true });
  await wait('typeof localRecent!=="undefined"&&!document.body.classList.contains("auth-locked")&&localRecent.length===6');
  await ev("setToken('')");
  assert.equal(await ev('localRecent.length'), 0);
  assert.equal(await ev('recentObjectURLs.size'), 0);
  await ev(`setToken(${JSON.stringify(cfg.auth_token)});initApp()`);
  await wait('localRecent.length===6');
  await wait('[...document.querySelectorAll("#recentGrid img")].every(img=>img.naturalWidth>0)');
  await ev('window.localImage=document.querySelector("#recentGrid img");window.blobURL=localImage.src;loadLocalRecent()');
  assert(await ev('document.querySelector("#recentGrid img")===localImage&&localImage.src===blobURL'));
  await ev('clearLocalRecent()');
  assert.equal(await ev('localRecent.length'), 0);

  // Deferred thumbnails may not resurrect cleared browser history.
  await ev(`(async()=>{const canvas=document.createElement('canvas');canvas.width=8;canvas.height=8;const blob=await new Promise(r=>canvas.toBlob(r,'image/png'));window.realThumb=cachedThumbnail;cachedThumbnail=()=>new Promise(r=>window.releaseThumb=r);window.late=rememberLocalUpload({key:'late',name:'late.png',url:location.origin+'/i/late.png',sizeKB:1},new File([blob],'late.png',{type:'image/png'}));await clearLocalRecent();releaseThumb(blob);await late;await loadLocalRecent();cachedThumbnail=realThumb;})()`);
  assert.equal(await ev('localRecent.length'), 0);
  await ev('state.queue.forEach(q=>URL.revokeObjectURL(q.preview));state.queue=[];renderQueue();showPage("gallery")');
  console.log('PASS 7 rejected wholesale, six browser-mocked uploads at peak3, IndexedDB original names/blob reuse, clear race blocked');

  let collect = null;
  if (probePath && fs.existsSync(probePath)) {
    const source = fs.readFileSync(probePath, 'utf8');
    collect = new Function(source.slice(source.indexOf('const COLLECT ='), source.indexOf('ws.onopen =')) + ';return COLLECT;')();
  }
  const layouts = [];
  for (const theme of ['light', 'dark']) {
    for (const view of ['gallery', 'sortmenu', 'inspector', 'batch', 'upload', 'api', 'settings', 'telegram', 'fullscreen', 'confirm', 'password']) {
      await ev(`closeConfirm(false);closeRecentZoom();closePwdModal();setSortMenuOpen(false);if(document.querySelector('#tgDialog').open)document.querySelector('#tgDialog').close();state.selectMode=false;state.selected.clear();showPage('gallery');closeInspector();applyThemeMode('${theme}');`);
      if (view === 'sortmenu') await ev('setSortMenuOpen(true)');
      if (['upload', 'api', 'settings'].includes(view)) await ev(`showPage('${view}')`);
      if (view === 'telegram') {
        await ev('showPage("settings");document.querySelector("#tgManage").click()');
        await wait('tgSettings!==null');
      }
      if (view === 'password') await ev('showPage("settings");openPwdModal()');
      if (view === 'inspector') await ev('openInspector(items[0].id)');
      if (view === 'batch') await ev('state.selectMode=true;state.selected.add(items[0].id);updateGallerySelection()');
      if (view === 'fullscreen' || view === 'confirm') await ev('openInspector(items[0].id)');
      if (view === 'fullscreen') await ev('setInspectorFullscreen(true)');
      if (view === 'confirm') {
        await ev('document.querySelector("#inspDeleteBtn").click();true');
        await wait('document.querySelector("#confirmOverlay").classList.contains("show")');
      }
      for (const width of [320, 375, 414, 768, 1024, 1440, 1920]) {
        await S('Emulation.setDeviceMetricsOverride', { width, height: 950, deviceScaleFactor: 1, mobile: false });
        await sleep(80);
        if (view === 'inspector') {
          const modal = await ev('(()=>{const r=document.querySelector("#inspector").getBoundingClientRect();return {x:r.x,y:r.y,w:r.width,h:r.height,ww:innerWidth,hh:innerHeight,hidden:document.querySelector("#inspectorModal").hidden,inert:document.querySelector(".canvas").inert};})()');
          assert(Math.abs(modal.x + modal.w / 2 - modal.ww / 2) < 2 && Math.abs(modal.y + modal.h / 2 - modal.hh / 2) < 2 && !modal.hidden && modal.inert, 'image modal is not centered ' + JSON.stringify(modal));
        }
        if (collect) {
          const report = await ev(collect(false));
          const realDefects = report.defects.filter((d) => !(d.type === 'element_overflow' && d.selector === 'div#mobileDateStrip.mobile-filter-strip'));
          layouts.push({ theme, view, width, ...report, defects: realDefects });
          const defects = realDefects.filter((d) => ['high', 'medium'].includes(d.severity));
          if (defects.length) console.log('LAYOUT', JSON.stringify({ theme, view, width, defects }));
        }
        if ((width === 375 || width === 1440) && ['gallery', 'sortmenu', 'inspector', 'settings', 'upload'].includes(view)) {
          const r = await S('Page.captureScreenshot', { format: 'png' });
          fs.writeFileSync(`${out}/${theme}-${view}-${width}.png`, Buffer.from(r.data, 'base64'));
        }
      }
    }
  }
  if (collect) {
    fs.writeFileSync(out + '/layouts.json', JSON.stringify(layouts, null, 2));
    assert.equal(layouts.reduce((n, l) => n + l.defects.filter((d) => ['high', 'medium'].includes(d.severity)).length, 0), 0, 'layout defects');
  }

  await ev('closeConfirm(false);closeRecentZoom();closePwdModal();if(document.querySelector("#tgDialog").open)document.querySelector("#tgDialog").close();window.realStatus=PicAPI.fetchStatus;PicAPI.fetchStatus=async()=>({unavailable:true});revalidateAuth()');
  assert(!await ev('document.body.classList.contains("auth-locked")'));
  await ev('PicAPI.fetchStatus=async()=>({has_auth:true,authenticated:false});revalidateAuth()');
  assert(await ev('document.body.classList.contains("auth-locked")&&items.length===0&&recentObjectURLs.size===0&&document.querySelector("#inspector").hidden'));
  assert.deepEqual(errors, []);
  assert.deepEqual(writes, []);
  console.log('PASS responsive states light/dark, invalid auth cleanup, offline preservation, no JS exceptions, no service mutation requests');
} finally {
  await send('Target.disposeBrowserContext', { browserContextId });
  ws.close();
}
