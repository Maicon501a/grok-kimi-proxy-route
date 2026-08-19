package cdp

// botguardHookScriptSource is injected before each navigation to capture the
// gyb callback from the Botguard runtime. It mirrors the original
// evaluateOnNewDocument hook.
const botguardHookScriptSource = `(() => {
  if (window.__codexHookBootstrapInstalled) return;
  window.__codexHookBootstrapInstalled = true;
  // Reporter para o interpretador Botguard instrumentado (debug): as catches
  // da VM chamam __bgErr; roda tambem no realm do iframe bscframe (mesma
  // origem, entao da para escrever no top). Inerte sem o loader instrumentado.


  window.__bgErr = function (e, pos) {
    try {
      const top = window.top || window;
      top.__bgErrLog = top.__bgErrLog || [];
      if (top.__bgErrLog.length < 200) {
        top.__bgErrLog.push((e && e.constructor ? e.constructor.name : '?') + ': ' + String(e && e.message || e).slice(0, 120) + ' @' + pos);
      }
    } catch (err) {}
  };
  window.__codexBgCallLog = window.__codexBgCallLog || [];
  const describeArg = (arg) => {
    if (typeof arg === 'function') return { t: 'fn', src: String(arg).slice(0, 120) };
    try { return { t: typeof arg, v: JSON.stringify(arg) && JSON.stringify(arg).slice(0, 60000) }; }
    catch (e) { return { t: typeof arg, v: String(arg).slice(0, 120) }; }
  };
  const wrapMethod = (methodName, original) => {
    if (typeof original !== 'function' || original.__codexWrapped) return original;
    const wrapped = function (...args) {
      try {
        if (window.__codexBgCallLog.length > 50) window.__codexBgCallLog.length = 0;
        window.__codexBgCallLog.push({
          m: methodName,
          at: Date.now(),
          thisType: this && this.constructor ? this.constructor.name : typeof this,
          args: args.map(describeArg),
          stack: (new Error('trace')).stack ? String((new Error('trace')).stack).slice(0, 600) : '',
        });
      } catch (e) {}
      const effectiveArgs = args.map((arg, index) => {
        if (typeof arg !== 'function') return arg;
        return function (...innerArgs) {
          if ((methodName === 'a' || methodName === 'ZOI_') && index === 4) {
            try {
              window.__codexBgTelemetry = window.__codexBgTelemetry || [];
              if (window.__codexBgTelemetry.length > 200) window.__codexBgTelemetry.length = 0;
              window.__codexBgTelemetry.push(Array.from(innerArgs).map((v) => {
                if (typeof v === 'function') return 'fn';
                try { return JSON.parse(JSON.stringify(v)); } catch (e) { return String(v); }
              }));
            } catch (e) {}
          }
          if ((methodName === 'a' || methodName === 'ZOI_') && index === 1) {
            if (typeof innerArgs[0] === 'function') {
              const gybFn = innerArgs[0];
              const gybThis = this;
              window.__codexCapturedGyb = function (...gybArgs) {
                return gybFn.apply(gybThis, gybArgs);
              };
              window.__codexCapturedGybMeta = {
                methodName,
                capturedAt: Date.now(),
                thisType: gybThis && gybThis.constructor ? gybThis.constructor.name : typeof gybThis,
              };
            }
          }
          return arg.apply(this, innerArgs);
        };
      });
      return original.apply(this, effectiveArgs);
    };
    wrapped.__codexWrapped = true;
    return wrapped;
  };
  const patchBotguard = (target) => {
    if (!target || target.__codexPatchedObject) return target;
    target.__codexPatchedObject = true;
    const installTrap = (methodName) => {
      let currentValue = target[methodName];
      Object.defineProperty(target, methodName, {
        configurable: true, enumerable: true,
        get() { return currentValue; },
        set(nextValue) { currentValue = wrapMethod(methodName, nextValue); },
      });
      if (currentValue !== undefined) target[methodName] = currentValue;
    };
    installTrap('a');
    installTrap('ZOI_');
    return target;
  };
  let botguardValue = window.botguard;
  if (botguardValue) botguardValue = patchBotguard(botguardValue);
  Object.defineProperty(window, 'botguard', {
    configurable: true, enumerable: true,
    get() { return botguardValue; },
    set(nextValue) { botguardValue = patchBotguard(nextValue); },
  });
  // Captura o body do CountTokens/GenerateContent: Chrome/app novos enviam o
  // body como ReadableStream (upload streamed) e o CDP NAO expoe postData —
  // nem em requestWillBeSent nem via getRequestPostData. Lemos aqui no JS
  // (tee do stream, sem consumir o original) e o bootstrap combina com os
  // headers do evento de rede.
  (function installCtCapture() {
    try {
      const store = (window === window.top) ? window : (window.top || window);
      const origFetch = window.fetch;
      if (typeof origFetch !== 'function' || origFetch.__codexCt) return;
      const save = (url, text, bodyType) => {
        try {
          store.__codexCtCapture = { url: String(url), body: String(text), at: Date.now(), blen: String(text).length, btype: String(bodyType || '?'), frame: window === window.top ? 'top' : 'iframe' };
          store.__codexCtSaves = (store.__codexCtSaves || 0) + 1;
        } catch (e) {}
      };
      const readBody = (body, url) => {
        try {
          if (typeof body === 'string') { save(url, body, 'string'); return null; }
          if (typeof ArrayBuffer !== 'undefined' && ArrayBuffer.isView && ArrayBuffer.isView(body)) {
            try { save(url, new TextDecoder().decode(body), 'view'); } catch (e) {}
            return null;
          }
          if (typeof ArrayBuffer !== 'undefined' && body instanceof ArrayBuffer) {
            try { save(url, new TextDecoder().decode(body), 'buffer'); } catch (e) {}
            return null;
          }
          if (typeof ReadableStream !== 'undefined' && body instanceof ReadableStream) {
            const pair = body.tee();
            new Response(pair[0]).text().then((t) => save(url, t, 'stream')).catch(() => {});
            return pair[1];
          }
          if (typeof Blob !== 'undefined' && body instanceof Blob) {
            body.text().then((t) => save(url, t, 'blob')).catch(() => {});
            return null;
          }
        } catch (e) {}
        return null;
      };
      const wrapped = function (input, init) {
        try {
          const url = typeof input === 'string' ? input : (input && input.url) || '';
          if (url.indexOf('MakerSuiteService/CountTokens') >= 0 || url.indexOf('MakerSuiteService/GenerateContent') >= 0) {
            store.__codexCtSeen = (store.__codexCtSeen || 0) + 1;
            store.__codexCtLastSeen = { url: String(url).slice(0, 90), hasInit: !!init, bodyType: init && init.body != null ? (typeof init.body === 'string' ? 'string' : (init.body && init.body.constructor ? init.body.constructor.name : typeof init.body)) : (typeof Request !== 'undefined' && input instanceof Request ? 'Request' : 'none'), at: Date.now(), frame: window === window.top ? 'top' : 'iframe' };
            if (init && init.body != null) {
              const repl = readBody(init.body, url);
              if (repl) init = Object.assign({}, init, { body: repl });
            } else if (typeof Request !== 'undefined' && input instanceof Request) {
              try { input.clone().text().then((t) => save(url, t, 'request')).catch((e) => save(url, '', 'request-clone-err:' + e)); } catch (e) { save(url, '', 'request-err:' + e); }
            }
          }
        } catch (e) {}
        return origFetch.call(this, input, init);
      };
      wrapped.__codexCt = true;
      window.fetch = wrapped;
      // o cliente RPC do AI Studio pode usar XHR em vez de fetch
      if (typeof XMLHttpRequest !== 'undefined' && !XMLHttpRequest.prototype.__codexCt) {
        const origOpen = XMLHttpRequest.prototype.open;
        const origSend = XMLHttpRequest.prototype.send;
        XMLHttpRequest.prototype.open = function (method, url) {
          try { this.__codexUrl = String(url); } catch (e) {}
          return origOpen.apply(this, arguments);
        };
        XMLHttpRequest.prototype.send = function (body) {
          try {
            const url = this.__codexUrl || '';
            if (url.indexOf('MakerSuiteService/CountTokens') >= 0 || url.indexOf('MakerSuiteService/GenerateContent') >= 0) {
              if (typeof body === 'string') save(url, body, 'xhr-string');
              else if (body && typeof ArrayBuffer !== 'undefined' && ArrayBuffer.isView && ArrayBuffer.isView(body)) { try { save(url, new TextDecoder().decode(body), 'xhr-view'); } catch (e) {} }
              else if (body && typeof Blob !== 'undefined' && body instanceof Blob) body.text().then((t) => save(url, t, 'xhr-blob')).catch(() => {});
              else if (body && body instanceof ArrayBuffer) { try { save(url, new TextDecoder().decode(body), 'xhr-buffer'); } catch (e) {} }
            }
          } catch (e) {}
          return origSend.apply(this, arguments);
        };
        XMLHttpRequest.prototype.__codexCt = true;
      }
    } catch (e) {}
  })();
})();`

const fillPromptExpr = `(() => {
  const target =
    document.querySelector('textarea[aria-label="Enter a prompt"]') ||
    document.querySelector('textarea') ||
    document.querySelector('[contenteditable="true"]') ||
    document.querySelector('[role="textbox"]');
  if (!target) return false;
  target.focus();
  const content = %s;
  if (target.tagName === 'TEXTAREA') {
    const setter = Object.getOwnPropertyDescriptor(window.HTMLTextAreaElement.prototype, 'value').set;
    setter.call(target, content);
    target.dispatchEvent(new Event('input', { bubbles: true }));
    target.dispatchEvent(new Event('change', { bubbles: true }));
  } else {
    target.textContent = content;
    target.dispatchEvent(new Event('input', { bubbles: true }));
  }
  return true;
})()`

const dismissUIExpr = `(() => {
  const visible = (el) => {
    if (!el) return false;
    const s = window.getComputedStyle(el);
    const r = el.getBoundingClientRect();
    return s.visibility !== 'hidden' && s.display !== 'none' && r.width > 0 && r.height > 0;
  };
  const norm = (v) => String(v || '').trim().toLowerCase();
  const buttons = Array.from(document.querySelectorAll('button, [role="button"]'));
  for (const label of ['skip','close','cancel','dismiss','not now']) {
    const btn = buttons.find((c) => {
      const t = norm(c.textContent); const a = norm(c.getAttribute && c.getAttribute('aria-label'));
      return visible(c) && (t === label || a === label);
    });
    if (btn) { btn.click(); return true; }
  }
  return false;
})()`

// botguardProbeRecorderSource registra TUDO que a VM le do ambiente (debug).
// Roda em todos os frames, inclusive o realm do iframe bscframe onde a VM
// executa. NAO vai para producao: wraps de Navigator.prototype etc. sao
// detectados pelo integrity check do Botguard (programa novo envenena o
// token -> 400/403 upstream).
const botguardProbeRecorderSource = `(() => {
  if (window.__codexProbeRecorderInstalled) return;
  window.__codexProbeRecorderInstalled = true;
  (function installProbeRecorder() {
    try {
      const top = window.top || window;
      top.__probeLog = top.__probeLog || [];
      const log = (entry) => {
        try { if (top.__probeLog.length < 4000) top.__probeLog.push(entry); } catch (e) {}
      };
      const seen = new Set();
      const wrapGetters = (proto, tag) => {
        if (!proto) return;
        let names;
        try { names = Object.getOwnPropertyNames(proto); } catch (e) { return; }
        for (const name of names) {
          if (name === 'constructor') continue;
          let desc;
          try { desc = Object.getOwnPropertyDescriptor(proto, name); } catch (e) { continue; }
          if (!desc || !desc.get || desc.configurable === false) continue;
          const origGet = desc.get;
          try {
            Object.defineProperty(proto, name, {
              configurable: true,
              enumerable: desc.enumerable,
              get: function () {
                const key = tag + '.' + name;
                if (!seen.has(key)) { seen.add(key); log(key); }
                return origGet.call(this);
              },
            });
          } catch (e) {}
        }
      };
      wrapGetters(window.Navigator && Navigator.prototype, 'navigator');
      wrapGetters(window.Screen && Screen.prototype, 'screen');
      wrapGetters(window.ScreenOrientation && ScreenOrientation.prototype, 'orientation');
      wrapGetters(window.Performance && Performance.prototype, 'performance');
      wrapGetters(window.Storage && Storage.prototype, 'storage');
      // metodos de document que a VM usa
      const docProto = window.Document && Document.prototype;
      if (docProto) {
        for (const name of ['createElement', 'createElementNS', 'querySelector', 'getElementById', 'getElementsByTagName', 'elementFromPoint', 'hasFocus', 'createEvent']) {
          const orig = docProto[name];
          if (typeof orig !== 'function') continue;
          try {
            Object.defineProperty(docProto, name, {
              configurable: true, writable: true,
              value: function (...args) {
                const key = 'document.' + name + '(' + args.map(String).join(',').slice(0, 60) + ')';
                if (!seen.has(key)) { seen.add(key); log(key); }
                return orig.apply(this, args);
              },
            });
          } catch (e) {}
        }
      }
      // window-level props
      for (const name of ['devicePixelRatio', 'innerWidth', 'innerHeight', 'outerWidth', 'outerHeight', 'screenX', 'screenY', 'pageXOffset', 'pageYOffset', 'chrome', 'external', 'visualViewport', 'speechSynthesis', 'indexedDB', 'caches', 'scheduler', 'crossOriginIsolated', 'isSecureContext', 'origin']) {
        try {
          const cur = window[name];
          Object.defineProperty(window, name, {
            configurable: true,
            get: function () {
              const key = 'window.' + name;
              if (!seen.has(key)) { seen.add(key); log(key); }
              return cur;
            },
          });
        } catch (e) {}
      }
    } catch (e) {}
  })();
})();`
