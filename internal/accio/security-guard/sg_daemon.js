/**
 * Security Guard persistent daemon.
 *
 * Protocol: JSON lines on stdin → JSON lines on stdout.
 * Request:  {"id":"req-1","url":"https://..."}
 * Response: {"id":"req-1","headers":{"pctb-x-sign":"...",...}}
 * Error:    {"id":"req-1","error":"message"}
 *
 * Startup: prints {"ready":true} to stdout once UMID is initialized.
 * The Go side reads this to know the daemon is ready.
 */
"use strict";

const path = require("path");
const readline = require("readline");

// Suppress all console output to stderr (keep stdout clean for protocol)
console.log = (...args) => process.stderr.write(args.map(String).join(" ") + "\n");
console.warn = (...args) => process.stderr.write(args.map(String).join(" ") + "\n");
console.error = (...args) => process.stderr.write(args.map(String).join(" ") + "\n");

const APPKEY = process.env.ACCIO_SG_APPKEY || "35336201";

// Resolve the native addon relative to this script
const addonPath = path.join(__dirname, "prebuild", "win32-x64", "security_guard.node");

let addon = null;
let ready = false;

function loadAddon() {
  try {
    addon = require(addonPath);
    console.log("[sg-daemon] addon loaded from", addonPath);
  } catch (err) {
    process.stdout.write(JSON.stringify({ ready: false, error: "addon load failed: " + err.message }) + "\n");
    process.exit(1);
  }
}

async function initUmid() {
  try {
    await addon.initUmid(6); // AREA_ONLINE
    console.log("[sg-daemon] UMID initialized");
  } catch (err) {
    if (/errorCode=1/.test(err.message)) {
      console.log("[sg-daemon] UMID already initialized (cross-process)");
    } else {
      console.log("[sg-daemon] UMID init warning:", err.message);
    }
  }
  ready = true;
  process.stdout.write(JSON.stringify({ ready: true }) + "\n");
}

function getHeaders(url) {
  const input = JSON.stringify({ appkey: APPKEY, urlInput: url });
  const raw = addon.getSecurityFactorsForWeb(input);
  const parsed = JSON.parse(raw);
  const headers = {};
  for (const [k, v] of Object.entries(parsed)) {
    if (typeof v === "string") headers[k] = encodeURIComponent(v);
    else if (typeof v === "number" || typeof v === "boolean") headers[k] = encodeURIComponent(String(v));
  }
  return headers;
}

function handleLine(line) {
  const trimmed = line.trim();
  if (!trimmed) return;

  let req;
  try {
    req = JSON.parse(trimmed);
  } catch {
    process.stdout.write(JSON.stringify({ id: null, error: "invalid JSON" }) + "\n");
    return;
  }

  const { id, url } = req;

  if (!ready) {
    process.stdout.write(JSON.stringify({ id, error: "daemon not ready" }) + "\n");
    return;
  }

  if (!url || typeof url !== "string") {
    process.stdout.write(JSON.stringify({ id, error: "missing url" }) + "\n");
    return;
  }

  try {
    const headers = getHeaders(url);
    process.stdout.write(JSON.stringify({ id, headers }) + "\n");
  } catch (err) {
    process.stdout.write(JSON.stringify({ id, error: err.message }) + "\n");
  }
}

// ─── Main ────────────────────────────────────────────────────────────────────
loadAddon();

const rl = readline.createInterface({ input: process.stdin, terminal: false });
rl.on("line", handleLine);
rl.on("close", () => {
  console.log("[sg-daemon] stdin closed, exiting");
  process.exit(0);
});

// Initialize UMID async, then signal ready
initUmid();
