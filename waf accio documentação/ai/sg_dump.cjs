"use strict";
// Mass-dump harness for SecurityGuard addon — controlled variable isolation.
// Usage: set SG_ADDON to the .node path, SG_DLLS to the x64 dir, then run.
const fs = require("fs");
const path = require("path");
const crypto = require("crypto");

const addonPath = process.env.SG_ADDON || "";
const addon = require(addonPath);

console.log("[dump] addon keys:", Object.keys(addon).join(", "));
console.log("[dump] isStub:", addon.isStub);

const OUT = process.env.SG_OUT || path.join(__dirname, "sg_dump.jsonl");
const lines = [];
const rec = (obj) => {
  const line = JSON.stringify(obj);
  lines.push(line);
  fs.appendFileSync(OUT, line + "\n");
  console.log(line.slice(0, 300) + (line.length > 300 ? " …" : ""));
};

function factors(url, appkey) {
  const input = JSON.stringify({ appkey, urlInput: url });
  const raw = addon.getSecurityFactorsForWeb(input);
  const parsed = JSON.parse(raw);
  const out = { url, appkey, ts: Math.floor(Date.now() / 1000) };
  for (const k of Object.keys(parsed)) {
    const v = parsed[k];
    out[k] = typeof v === "string" ? v : v;
  }
  if (out["pctb-x-sign"]) out["sign_len"] = out["pctb-x-sign"].length;
  if (out["pctb-x-mini-wua"]) out["wua_len"] = out["pctb-x-mini-wua"].length;
  return out;
}

async function main() {
  await addon.initUmid(6);
  console.log("[dump] initUmid(6) ok\n");

  const K = process.env.SG_APPKEY || "35336201";

  // ---- Exp 1: same URL, 5 sequential calls (time-dependence of sign) ----
  const baseUrl = "https://phoenix-gw.alibaba.com/api/adk/llm/generateContent?sg_k=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
  rec({ exp: "1-same-url-x5", note: "same URL, consecutive calls" });
  for (let i = 0; i < 5; i++) rec(factors(baseUrl, K));

  // ---- Exp 2: URL variation — path only changes ----
  const paths = [
    "/api/adk/llm/generateContent",
    "/api/rc/pc/token",
    "/api/auth/safe/refresh_token",
    "/api/oauth/token",
    "/api/tool/featureFlag/evaluate",
    "/api/entitlement/quota",
    "/api/user/login",
  ];
  rec({ exp: "2-path-variation", note: "same host, different paths" });
  for (const p of paths) {
    rec(factors("https://phoenix-gw.alibaba.com" + p + "?sg_k=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", K));
  }

  // ---- Exp 3: host variation ----
  rec({ exp: "3-host-variation", note: "same path, different hosts" });
  const hosts = [
    "https://phoenix-gw.alibaba.com/api/adk/llm/generateContent?sg_k=cccccccccccccccccccccccccccccccc",
    "https://www.accio.com/api/adk/llm/generateContent?sg_k=cccccccccccccccccccccccccccccccc",
    "https://filebroker.accio.com/api/adk/llm/generateContent?sg_k=cccccccccccccccccccccccccccccccc",
    "http://localhost:4097/api/adk/llm/generateContent?sg_k=cccccccccccccccccccccccccccccccc",
  ];
  for (const u of hosts) rec(factors(u, K));

  // ---- Exp 4: query variation — same path, different sg_k / params ----
  rec({ exp: "4-query-variation", note: "same path, different query" });
  const queries = [
    "https://phoenix-gw.alibaba.com/api/adk/llm/generateContent?sg_k=dddddddddddddddddddddddddddddddd",
    "https://phoenix-gw.alibaba.com/api/adk/llm/generateContent?sg_k=eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
    "https://phoenix-gw.alibaba.com/api/adk/llm/generateContent?sg_k=ffffffffffffffffffffffffffffffff&x=1",
    "https://phoenix-gw.alibaba.com/api/adk/llm/generateContent",
    "https://phoenix-gw.alibaba.com/api/adk/llm/generateContent?",
  ];
  for (const u of queries) rec(factors(u, K));

  // ---- Exp 5: appkey variation (win vs mac) ----
  rec({ exp: "5-appkey-variation", note: "35336201 vs 35337600" });
  rec(factors("https://phoenix-gw.alibaba.com/api/adk/llm/generateContent?sg_k=abcdefabcdefabcdefabcdefabcdefab", "35336201"));
  rec(factors("https://phoenix-gw.alibaba.com/api/adk/llm/generateContent?sg_k=abcdefabcdefabcdefabcdefabcdefab", "35337600"));

  // ---- Exp 6: URL length sweep (padding / block-size detection) ----
  rec({ exp: "6-length-sweep", note: "grow path length to detect block size" });
  const base = "https://phoenix-gw.alibaba.com/api/";
  for (let n = 1; n <= 8; n++) {
    const url = base + "a".repeat(n * 16) + "?sg_k=" + "0".repeat(32);
    rec(factors(url, K));
  }

  // ---- Exp 7: no query, no path (root) ----
  rec({ exp: "7-root-urls", note: "bare hosts" });
  rec(factors("https://phoenix-gw.alibaba.com/", K));
  rec(factors("https://www.accio.com/", K));

  console.log("\n[dump] done. total records:", lines.length, "->", OUT);
}

main().catch((e) => { console.error("[dump] fatal:", e); process.exit(1); });
