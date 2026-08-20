#!/usr/bin/env node
/**
 * login_bridge.mjs — "browser as transport" bridge for the Go probe.
 *
 * JSON-lines protocol on stdin/stdout. The browser (real Chrome, headful)
 * holds ALL Google state (cookies, page instances, botguard VMs); Go builds
 * and parses every payload. Nothing is typed or clicked — pages are loaded
 * and RPCs are sent via in-page fetch(), byte-identical to the real flow.
 *
 * Commands:
 *   {"cmd":"start"}                          → launch browser
 *   {"cmd":"load","url":U}                   → goto U; resp {finalURL,status,html}
 *   {"cmd":"mint","binding":{...}}           → mint botguard token on CURRENT page
 *   {"cmd":"fetch","method":M,"url":U,"headers":{...},"body":B}
 *                                            → in-page fetch, credentials:'include';
 *                                              resp {status,body,respHeaders}
 *   {"cmd":"loadChain","url":U}              → goto U following redirects; captures
 *                                              loopback code; resp {finalURL,html,code}
 *   {"cmd":"close"}
 */
const { chromium } = require('playwright');
const fs = require('fs');
const path = require('path');
const readline = require('readline');

const log = (...m) => console.error(new Date().toISOString(), ...m); // stderr only
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function _parseDS(html, key) {
  const m = html.indexOf(`key: '${key}'`);
  if (m < 0) return null;
  const dp = html.indexOf('data:', m);
  const as = html.indexOf('[', dp);
  let d = 0, p = as, s = false, e = false;
  for (; p < html.length; p++) {
    const c = html[p];
    if (e) { e = false; continue; }
    if (c === '\\') { e = true; continue; }
    if (c === '"') { s = !s; continue; }
    if (s) continue;
    if (c === '[') d++;
    if (c === ']') { d--; if (d === 0) break; }
  }
  return JSON.parse(html.substring(as, p + 1).replace(/\\u003d/g, '=').replace(/\\u003c/g, '<').replace(/\\u003e/g, '>'));
}
function extractBfkj(html) {
  for (let i = 0; i <= 9; i++) {
    const ds = _parseDS(html, 'ds:' + i);
    if (!ds) continue;
    const s = JSON.stringify(ds);
    if (!s.includes('bfkj')) continue;
    const vm = ds && ds[4] && ds[4][1] ? ds[4][1][5] : '';
    const bm = s.match(/"([A-Za-z0-9+/=]{10000,})"/);
    if (typeof vm === 'string' && vm.length > 1000 && bm) return { vmCode: vm, program: bm[1] };
  }
  return null;
}

let context = null;
let page = null;
let loopbackCode = null;
let pwdBg = null; // {vmCode, program} captured from the page's own WZfWSd response

async function start() {
  return await newSession();
}

// newSession closes the current context (if any) and opens a FRESH isolated
// profile — one shared Chrome process serving many sequential logins, each
// with clean cookies/storage so Google accounts never bleed into each other.
async function newSession() {
  if (context) {
    try { await context.close(); } catch {}
    context = null;
    page = null;
  }
  loopbackCode = null;
  pwdBg = null;
  const profileDir = path.join(process.cwd(), 'browser-data', 'bridge-' + Date.now());
  context = await chromium.launchPersistentContext(profileDir, {
    channel: 'chrome',
    headless: false,
    viewport: { width: 1280, height: 800 },
    locale: 'pt-BR',
    ignoreHTTPSErrors: true,
    ignoreDefaultArgs: ['--enable-automation'],
    args: ['--disable-blink-features=AutomationControlled', '--no-first-run', '--window-position=-32000,-32000'],
  });
  await context.addInitScript(() => {
    Object.defineProperty(navigator, 'webdriver', { get: () => undefined });
  });
  page = context.pages()[0] || (await context.newPage());
  // capture loopback code without actually hitting 127.0.0.1
  // NOTE: must be ANCHORED — Google's CheckCookie/consent URLs embed the raw
  // loopback redirect_uri as a nested param, so an unanchored match would
  // hijack the CheckCookie navigation itself.
  await context.route(/^http:\/\/127\.0\.0\.1:\d+\/callback/, (route) => {
    try {
      const u = new URL(route.request().url());
      loopbackCode = u.searchParams.get('code');
      log('loopback hit: ' + u.toString().slice(0, 200));
    } catch {}
    return route.fulfill({ status: 200, contentType: 'text/html', body: 'ok' });
  });
  // observe the page's own WZfWSd multi-RPC response → password-page bg program
  await page.route('**/data/batchexecute**', async (route) => {
    if (!route.request().url().includes('zKAP2e')) return route.continue();
    try {
      const resp = await route.fetch();
      const body = await resp.text();
      const i = body.indexOf('"zKAP2e"');
      if (i >= 0) {
        // zKAP2e payload is a JSON string; find it and parse
        const chunkStart = body.lastIndexOf('[["wrb.fr"', i);
        const nl = body.indexOf('\n', i);
        const chunk = body.slice(chunkStart, nl > 0 ? nl : undefined);
        const arr = JSON.parse(chunk);
        for (const item of arr) {
          if (Array.isArray(item) && item[0] === 'wrb.fr' && item[1] === 'zKAP2e' && typeof item[2] === 'string') {
            const zk = JSON.parse(item[2]);
            const vm = zk?.[4]?.[1]?.[5];
            const pm = item[2].match(/"([A-Za-z0-9+/=]{10000,})"/);
            if (typeof vm === 'string' && vm.length > 1000 && pm) {
              pwdBg = { vmCode: vm, program: pm[1] };
              log('captured pwd bg program from page WZfWSd (' + vm.length + '/' + pm[1].length + ')');
            }
          }
        }
      }
      return route.fulfill({ response: resp, body });
    } catch (e) {
      log('wz intercept error:', e.message);
      return route.continue();
    }
  });
  return { ok: true };
}

async function load(url) {
  loopbackCode = null;
  const resp = await page.goto(url, { waitUntil: 'domcontentloaded', timeout: 90000 });
  await sleep(2000); // let page JS + bg VM boot
  log('load return: code=' + (loopbackCode ? 'SET' : 'null') + ' url=' + page.url().slice(0, 80));
  return {
    finalURL: page.url(),
    status: resp ? resp.status() : 0,
    html: await page.content(),
    code: loopbackCode,
  };
}

async function mint(binding) {
  const html = await page.content();
  const found = extractBfkj(html);
  if (!found) throw new Error('bfkj not found on current page: ' + page.url());
  return await mintWith({ vmCode: found.vmCode, program: found.program, binding });
}

async function mintWith({ vmCode, program, binding }) {
  const token = await page.evaluate(async ({ vmCode, program, binding }) => {
    (0, eval)(vmCode);
    const vm = window.botguard;
    if (!vm || !vm.a) throw new Error('botguard global missing');
    let fns = null;
    const setupCb = (a) => { fns = { asyncSnap: a }; };
    const noop = () => {};
    const p = vm.a(program, setupCb, true, undefined, noop, [[], []], undefined, false,
      [noop, noop, noop, noop, noop]);
    if (p && typeof p.then === 'function') await p;
    for (let i = 0; i < 100 && !fns; i++) await new Promise((r) => setTimeout(r, 100));
    if (!fns) throw new Error('vm setup callback never fired');
    return await new Promise((resolve, reject) => {
      const to = setTimeout(() => reject(new Error('snapshot timeout')), 20000);
      fns.asyncSnap((resp) => { clearTimeout(to); resolve(resp); }, [binding, undefined, undefined, undefined]);
    });
  }, { vmCode, program, binding });
  return { token: String(token) };
}

async function fetchInPage(cmd) {
  const r = await page.evaluate(async ({ method, url, headers, body }) => {
    const resp = await fetch(url, {
      method, headers, body,
      credentials: 'include',
      redirect: 'manual',
    });
    const text = await resp.text();
    const h = {};
    resp.headers.forEach((v, k) => { h[k] = v; });
    return { status: resp.status, body: text, respHeaders: h, type: resp.type };
  }, { method: cmd.method || 'GET', url: cmd.url, headers: cmd.headers || {}, body: cmd.body || undefined });
  return r;
}

const rl = readline.createInterface({ input: process.stdin });
const out = (id, payload) => process.stdout.write(JSON.stringify({ id, ...payload }) + '\n');

rl.on('line', async (line) => {
  let cmd;
  try { cmd = JSON.parse(line); } catch { return; }
  const id = cmd.id;
  try {
    let r;
    if (cmd.cmd === 'start') r = await start();
    else if (cmd.cmd === 'newSession') r = await newSession();
    else if (cmd.cmd === 'load') r = await load(cmd.url);
    else if (cmd.cmd === 'mint') r = await mint(cmd.binding);
    else if (cmd.cmd === 'mintWith') r = await mintWith(cmd);
    else if (cmd.cmd === 'pushState') {
      r = await page.evaluate((u) => { history.pushState({}, '', u); return { finalURL: location.href }; }, cmd.url);
    }
    else if (cmd.cmd === 'setSessionKey') {
      // sessionPrivateKey = privRaw(32) || pubRaw(65) as a Latin-1 string,
      // written by the page's JS before MI613e — the bg VM reads it.
      r = await page.evaluate(({ hex }) => {
        const bytes = new Uint8Array(hex.match(/../g).map((h) => parseInt(h, 16)));
        let s = '';
        for (const b of bytes) s += String.fromCharCode(b);
        sessionStorage.setItem('sessionPrivateKey', s);
        return { stored: sessionStorage.getItem('sessionPrivateKey').length };
      }, { hex: cmd.hex });
    }
    else if (cmd.cmd === 'getURL') r = { url: page.url() };
    else if (cmd.cmd === 'clickConsent') {
      // click the OAuth approve button; the page sends xyhAld itself and
      // navigates consent?as=… → 302 loopback. Record nav hops to catch the code.
      const hops = [];
      const onResp = (resp) => {
        try {
          if (resp.request().isNavigationRequest()) {
            const h = resp.headers();
            hops.push({ url: resp.url(), status: resp.status(), location: h['location'] || '' });
          }
        } catch {}
      };
      page.on('response', onResp);
      const btn = page.locator('button:has-text("Continuar"), button:has-text("Continue"), button:has-text("Allow"), button:has-text("Permitir")').first();
      await btn.waitFor({ state: 'visible', timeout: 30000 });
      await btn.click();
      const t0 = Date.now();
      let code = null;
      while (Date.now() - t0 < 30000) {
        for (const h of hops) {
          for (const cand of [h.location, h.url]) {
            if (cand && cand.startsWith('http://127.0.0.1')) {
              try { code = new URL(cand).searchParams.get('code'); } catch {}
            }
            if (code) break;
          }
          if (code) break;
        }
        if (code) break;
        await sleep(500);
      }
      page.off('response', onResp);
      r = { code, hops };
    }
    else if (cmd.cmd === 'navCapture') {
      // navigate but RECORD every main-frame response (status + location).
      // Redirect hops can't be route-intercepted, so the final hop to
      // 127.0.0.1 will fail with ERR_CONNECTION_REFUSED — expected; we only
      // need the recorded Location headers.
      const hops = [];
      const onResp = (resp) => {
        try {
          if (resp.request().isNavigationRequest()) {
            const h = resp.headers();
            hops.push({ url: resp.url(), status: resp.status(), location: h['location'] || '' });
          }
        } catch {}
      };
      page.on('response', onResp);
      try { await page.goto(cmd.url, { timeout: 30000 }); } catch (e) { log('navCapture goto (expected err):', e.message.split('\n')[0]); }
      page.off('response', onResp);
      r = { hops };
    }
    else if (cmd.cmd === 'html') r = { html: await page.content() };
    else if (cmd.cmd === 'waitLeave') {
      // poll until the URL moves on OR the loopback code gets captured
      const t0 = Date.now();
      while (Date.now() - t0 < (cmd.ms || 15000)) {
        if (loopbackCode) break;
        if (!page.url().includes(cmd.fragment)) break;
        await sleep(500);
      }
      log('waitLeave return: code=' + (loopbackCode ? 'SET(' + loopbackCode.length + ')' : 'null') + ' url=' + page.url().slice(0, 80));
      r = { url: page.url(), html: await page.content(), code: loopbackCode };
    }
    else if (cmd.cmd === 'getPwdProgram') r = { found: !!pwdBg, vmCode: pwdBg?.vmCode || '', program: pwdBg?.program || '' };
    else if (cmd.cmd === 'submitEmail') {
      // real page transition: fill email, press Enter, wait for the password field.
      // The page sends its own MI613e + WZfWSd and renders the real pwd DOM.
      const emailSel = 'input[type="email"], #identifierId';
      await page.waitForSelector(emailSel, { state: 'visible', timeout: 30000 });
      await page.click(emailSel);
      await page.fill(emailSel, cmd.email);
      await page.keyboard.press('Enter');
      await page.waitForSelector('input[type="password"]', { state: 'visible', timeout: 60000 });
      await sleep(3000); // let the page's own bg bootstrap settle
      r = { url: page.url(), hasPwdBg: !!pwdBg };
    }
    else if (cmd.cmd === 'fillPassword') {
      const sel = 'input[type="password"]';
      await page.waitForSelector(sel, { state: 'visible', timeout: 30000 });
      await page.click(sel);
      await page.fill(sel, cmd.password);
      await page.evaluate((pw) => {
        const el = document.querySelector('input[type="password"]');
        if (!el) return;
        el.focus();
        el.value = pw;
        el.dispatchEvent(new Event('input', { bubbles: true }));
        el.dispatchEvent(new Event('change', { bubbles: true }));
      }, cmd.password);
      r = { filled: true };
    }
    else if (cmd.cmd === 'fillEmail') {
      // simulate typing: the bg snapshot may read the input field's value
      const sel = 'input[type="email"], #identifierId';
      await page.click(sel).catch(() => {});
      await page.fill(sel, cmd.email).catch(() => {});
      // real keystroke events on top, so listeners see a human-ish path
      await page.evaluate((email) => {
        const el = document.querySelector('input[type="email"], #identifierId');
        if (!el) return;
        el.focus();
        el.dispatchEvent(new Event('input', { bubbles: true }));
        el.dispatchEvent(new Event('change', { bubbles: true }));
      }, cmd.email);
      r = { filled: true };
    }
    else if (cmd.cmd === 'fetch') r = await fetchInPage(cmd);
    else if (cmd.cmd === 'loadChain') r = await load(cmd.url);
    else if (cmd.cmd === 'close') { await context.close(); process.exit(0); }
    else throw new Error('unknown cmd ' + cmd.cmd);
    out(id, { ok: true, ...r });
  } catch (e) {
    out(id, { ok: false, error: e.message });
  }
});
