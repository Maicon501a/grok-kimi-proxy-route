#!/usr/bin/env node
/**
 * Kimi Browser Login — Playwright Chromium + perfil persistente.
 *
 * Fluxo:
 * 1. Abre Chromium com perfil persistente
 * 2. Navega para a URL OAuth
 * 3. Auto-clique só no chooser de conta / consent (nunca no form de e-mail/senha)
 * 4. Captura o authorization code do redirect (se vier)
 * 5. NÃO fecha sozinho — espera VOCÊ fechar a janela do browser
 * 6. Só então grava o --out e encerra
 */

import { chromium } from 'playwright';
import fs from 'fs';
import path from 'path';

function parseArgs(argv) {
  const args = {};
  for (let i = 2; i < argv.length; i++) {
    const k = argv[i];
    if (!k.startsWith('--')) continue;
    const key = k.replace(/^--/, '');
    const next = argv[i + 1];
    if (next && !next.startsWith('--')) {
      args[key] = next;
      i++;
    } else {
      args[key] = true;
    }
  }
  return args;
}

function log(...msg) {
  console.log(new Date().toISOString(), ...msg);
}

function errorLog(...msg) {
  console.error(new Date().toISOString(), ...msg);
}

async function sleep(ms) {
  return new Promise((r) => setTimeout(r, ms));
}

function parseHeadless(v) {
  if (v === true || v === 'true' || v === '1') return true;
  return false;
}

function tryCaptureFromURL(url, state) {
  if (!url || !(url.includes('127.0.0.1') || url.includes('localhost'))) {
    return;
  }
  try {
    const u = new URL(url);
    const code = u.searchParams.get('code');
    const error = u.searchParams.get('error');
    if (code && !state.code) {
      log('Captured authorization code (browser stays open — feche a janela quando terminar)');
      state.code = code;
    }
    if (error && !state.error) {
      errorLog('OAuth error in URL:', error);
      state.error = error;
    }
  } catch (_) {
    // ignore
  }
}

async function fillGoogleLoginForm(page, email, password) {
  if (!email && !password) return false;
  try {
    // Email step
    const emailInput = await page.$('input[type="email"], input[name="identifier"], #identifierId');
    if (email && emailInput && (await emailInput.isVisible().catch(() => false))) {
      log('Auto-login: filling email...');
      await emailInput.fill(email);
      await sleep(500);
      const nextBtn = await page.$('button:has-text("Next"), button:has-text("Próxima"), button:has-text("Avançar"), #identifierNext, button[type="submit"]');
      if (nextBtn) {
        await nextBtn.click({ timeout: 5000 });
      } else {
        await page.keyboard.press('Enter');
      }
      await sleep(2500);
    }
    // Password step
    const passInput = await page.$('input[type="password"], input[name="Passwd"], input[name="password"]');
    if (password && passInput && (await passInput.isVisible().catch(() => false))) {
      log('Auto-login: filling password...');
      await passInput.fill(password);
      await sleep(500);
      const nextBtn = await page.$('button:has-text("Next"), button:has-text("Próxima"), button:has-text("Avançar"), #passwordNext, button[type="submit"]');
      if (nextBtn) {
        await nextBtn.click({ timeout: 5000 });
      } else {
        await page.keyboard.press('Enter');
      }
      await sleep(2500);
      return true;
    }
  } catch (e) {
    errorLog('Auto-login failed:', e.message);
  }
  return false;
}

async function isManualLoginForm(page) {
  try {
    const email = await page.$('input[type="email"], input[name="identifier"], #identifierId');
    if (email && (await email.isVisible().catch(() => false))) return true;
    const password = await page.$(
      'input[type="password"], input[name="Passwd"], input[name="password"]'
    );
    if (password && (await password.isVisible().catch(() => false))) return true;
    const totp = await page.$('input[type="tel"], input[name="totpPin"], #totpPin, #idvPin');
    if (totp && (await totp.isVisible().catch(() => false))) return true;
  } catch (_) {
    // ignore
  }
  return false;
}

async function clickAccountChooserOnce(page, preferredEmail) {
  const selectors = [
    'div[data-identifier]',
    'div[data-email]',
    '[role="link"][data-identifier]',
    '[role="button"][data-identifier]',
    'li[data-identifier]',
  ];
  for (const sel of selectors) {
    const els = await page.$$(sel);
    if (els.length === 0) continue;
    const visible = [];
    for (const el of els) {
      try {
        if (!(await el.isVisible())) continue;
        const id =
          (await el.getAttribute('data-identifier')) ||
          (await el.getAttribute('data-email')) ||
          '';
        visible.push({ el, id });
      } catch (_) {
        // try the next candidate
      }
    }
    const preferred = String(preferredEmail || '').trim().toLowerCase();
    const candidates = preferred
      ? [...visible.filter(({ id }) => id.toLowerCase() === preferred), ...visible.filter(({ id }) => id.toLowerCase() !== preferred)]
      : visible;
    for (const { el, id } of candidates) {
      try {
        log(`Account chooser: click once (${sel}${id ? ` · ${id}` : ''})`);
        await el.click({ timeout: 5000 });
        return true;
      } catch (e) {
        errorLog('account click failed:', e.message);
      }
    }
  }
  return false;
}

async function clickConsentOnce(page) {
  if (await isManualLoginForm(page)) return false;
  const selectors = [
    '#submit_approve_access',
    'button:has-text("Continuar")',
    'button:has-text("Continue")',
    'button:has-text("Allow")',
    'button:has-text("Permitir")',
    'button:has-text("Confirmar")',
  ];
  for (const sel of selectors) {
    try {
      const btn = await page.$(sel);
      if (!btn) continue;
      if (!(await btn.isVisible().catch(() => false))) continue;
      log('Consent: click once →', sel);
      await btn.click({ timeout: 5000 });
      return true;
    } catch (e) {
      errorLog('consent click failed:', e.message);
    }
  }
  return false;
}

async function main() {
  const args = parseArgs(process.argv);
  const authURL = args.authurl || args.authURL || '';
  const outPath = args.out || path.join(process.cwd(), 'kimi-session-out.json');
  const profileDir =
    args.profile || path.join(process.cwd(), 'browser-data', 'google-profile');
  const timeoutSec = parseInt(args.timeout || '600', 10);
  const headless = parseHeadless(args.headless);
  const autoClose = args['auto-close'] === true || args['auto-close'] === 'true' || args['auto-close'] === '1';
  const googleEmail = args.email || '';
  const googlePassword = args.password || '';

  if (!authURL) {
    errorLog('ERROR: --authurl required');
    process.exit(1);
  }

  log('=== Kimi Browser Login ===');
  log('profile:', profileDir);
  log('out:', outPath);
  log('headless:', headless);
    log('timeout:', timeoutSec, 's');
    if (autoClose) {
      log('auto-close: ativado — browser será fechado automaticamente após login.');
    } else {
      log('IMPORTANTE: o script NÃO fecha o browser sozinho.');
      log('Feche a janela do Playwright quando terminar o login — aí ele salva o resultado.');
    }

  fs.mkdirSync(profileDir, { recursive: true });
  fs.mkdirSync(path.dirname(outPath), { recursive: true });

  let context;
  try {
    log('Launching Playwright Chromium...');
    context = await chromium.launchPersistentContext(profileDir, {
      headless,
      viewport: { width: 1280, height: 800 },
      locale: 'pt-BR',
      timezoneId: 'America/Sao_Paulo',
      ignoreHTTPSErrors: true,
      ignoreDefaultArgs: ['--enable-automation'],
      args: [
        '--disable-blink-features=AutomationControlled',
        '--no-first-run',
        '--no-default-browser-check',
        '--disable-infobars',
        '--window-size=1280,800',
      ],
    });

    await context.addInitScript(() => {
      Object.defineProperty(navigator, 'webdriver', {
        get: () => undefined,
      });
    });

    const page = context.pages()[0] || (await context.newPage());
    const state = { code: null, error: null };

    page.on('framenavigated', (frame) => tryCaptureFromURL(frame.url(), state));
    page.on('request', (req) => tryCaptureFromURL(req.url(), state));

    // Also watch new pages/popups for the callback
    context.on('page', (p) => {
      p.on('framenavigated', (frame) => tryCaptureFromURL(frame.url(), state));
      p.on('request', (req) => tryCaptureFromURL(req.url(), state));
    });

    log('Navigating to Google OAuth...');
    await page.goto(authURL, {
      waitUntil: 'domcontentloaded',
      timeout: Math.min(timeoutSec, 120) * 1000,
    });
    log('Loaded:', page.url().substring(0, 140));

    // Promise that resolves when user closes the browser / all pages go away
    const browserClosed = new Promise((resolve) => {
      context.on('close', () => {
        log('Browser context closed by user (or process).');
        resolve('closed');
      });
    });

    // Light assist loop — never closes browser; stops after one account + one consent click
    const assistDeadline = Date.now() + Math.min(timeoutSec, 300) * 1000;
    let accountClicked = false;
    let consentClicked = false;
    let lastHandsOffLog = 0;

    const assistLoop = (async () => {
      while (Date.now() < assistDeadline) {
        // If browser already closed, stop
        if (!context.browser()?.isConnected() && context.pages().length === 0) {
          return;
        }
        try {
          const pages = context.pages();
          if (pages.length === 0) return;
          const p = pages[pages.length - 1];
          tryCaptureFromURL(p.url(), state);

          if (await isManualLoginForm(p)) {
            // If credentials provided, try auto-fill
            if (googleEmail || googlePassword) {
              const filled = await fillGoogleLoginForm(p, googleEmail, googlePassword);
              if (filled) {
                accountClicked = true;
                await sleep(2000);
                continue;
              }
            }
            const now = Date.now();
            if (now - lastHandsOffLog > 5000) {
              log('Hands-off: formulário de login/2FA — digite à vontade. Feche a janela quando terminar.');
              lastHandsOffLog = now;
            }
            await sleep(800);
            continue;
          }

          if (!accountClicked) {
            if (await clickAccountChooserOnce(p, googleEmail)) {
              accountClicked = true;
              await sleep(2000);
              continue;
            }
          }

          if (!consentClicked) {
            if (await clickConsentOnce(p)) {
              consentClicked = true;
              await sleep(2000);
              continue;
            }
          }

          if (state.code && !state._codeLogged) {
            state._codeLogged = true;
            if (autoClose) {
              log('Code OAuth capturado. Fechando browser automaticamente (auto-close)...');
            } else {
              log('Code OAuth capturado. Pode fechar a janela do browser para SALVAR a conta.');
            }
          }
          if (autoClose && state.code) {
            log('Auto-close: login concluído — fechando browser em 3s...');
            await sleep(3000);
            try { await context.close(); } catch (_) {}
            return;
          }
        } catch (_) {
          // page may have closed mid-loop
        }
        await sleep(1000);
      }
      log('Assist loop timed out — still waiting for you to close the browser window…');
    })();

    // Hard timeout overall
    const hardTimeout = sleep(timeoutSec * 1000).then(() => 'timeout');

    const winner = await Promise.race([browserClosed, hardTimeout]);
    // let assist finish quietly
    await Promise.race([assistLoop, sleep(100)]);

    // Final capture attempt from any remaining page URL (if still open)
    try {
      for (const p of context.pages()) {
        tryCaptureFromURL(p.url(), state);
      }
    } catch (_) {
      // ignore
    }

    // Ensure context closed
    try {
      await context.close();
    } catch (_) {
      // already closed
    }

    if (winner === 'timeout' && !state.code && !state.error) {
      errorLog('Timeout: você não fechou o browser e nenhum code foi capturado.');
      process.exit(1);
    }

    if (state.error) {
      fs.writeFileSync(
        outPath,
        JSON.stringify({ error: state.error, source: 'google_oauth' }, null, 2)
      );
      errorLog('OAuth error saved:', state.error);
      process.exit(1);
    }

    if (!state.code) {
      errorLog('Browser fechado sem authorization code. Faça o login até o redirect e feche depois.');
      process.exit(1);
    }

    fs.writeFileSync(
      outPath,
      JSON.stringify(
        {
          code: state.code,
          captured_at: new Date().toISOString(),
          source: 'playwright_chromium_profile',
          saved_on: 'browser_close',
        },
        null,
        2
      )
    );
    log('Saved on browser close →', outPath);
    process.exit(0);
  } catch (err) {
    errorLog('FATAL:', err.message);
    if (err.stack) errorLog(err.stack);
    if (context) {
      try {
        await context.close();
      } catch (_) {
        // ignore
      }
    }
    process.exit(1);
  }
}

main();
