"use strict";
// Fine-grained dump: time spacing + 1-byte URL mutation + component probe.
const fs = require("fs");
const path = require("path");
const addon = require(process.env.SG_ADDON || "");
const OUT = process.env.SG_OUT || path.join(__dirname, "sg_fine.jsonl");
const rec = (o) => { const l = JSON.stringify(o); fs.appendFileSync(OUT, l + "\n"); console.log(l.slice(0, 260) + (l.length > 260 ? " …" : "")); };

function factors(url, appkey) {
  const raw = addon.getSecurityFactorsForWeb(JSON.stringify({ appkey, urlInput: url }));
  const p = JSON.parse(raw);
  const out = { url, appkey, ms: Date.now(), ts: Math.floor(Date.now() / 1000) };
  for (const k of Object.keys(p)) out[k] = p[k];
  if (out["pctb-x-sign"]) { out["blob"] = Buffer.from(out["pctb-x-sign"], "base64").toString("hex"); }
  return out;
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function main() {
  await addon.initUmid(6);
  const K = process.env.SG_APPKEY || "35336201";
  const U = "https://phoenix-gw.alibaba.com/api/adk/llm/generateContent?sg_k=ffffffffffffffffffffffffffffffff";

  // Exp A: same URL, spaced 1.1s apart x3 (see if timestamp enters the sign)
  rec({ exp: "A-time-spacing", note: "same URL, 1.1s apart" });
  for (let i = 0; i < 3; i++) { rec(factors(U, K)); await sleep(1100); }

  // Exp B: 1-byte mutation sweep on the URL path (avalanche vs linear test)
  rec({ exp: "B-1byte-mutation", note: "single char flips in path" });
  const flips = [
    "https://phoenix-gw.alibaba.com/api/adk/llm/generateContent?sg_k=ffffffffffffffffffffffffffffffff",
    "https://phoenix-gw.alibaba.com/api/adk/llm/generateContenT?sg_k=ffffffffffffffffffffffffffffffff",
    "https://phoenix-gw.alibaba.com/api/adk/llm/generateContenx?sg_k=ffffffffffffffffffffffffffffffff",
    "https://phoenix-gw.alibaba.com/api/adk/llm/generateConten?sg_k=ffffffffffffffffffffffffffffffff",
    "https://phoenix-gw.alibaba.com/api/adk/llm/generateContenX?sg_k=ffffffffffffffffffffffffffffffff",
  ];
  for (const u of flips) rec(factors(u, K));

  // Exp C: query param flip only (sg_k value changes)
  rec({ exp: "C-query-flip", note: "sg_k value + extra param" });
  const qs = [
    "https://phoenix-gw.alibaba.com/api/adk/llm/generateContent?sg_k=ffffffffffffffffffffffffffffffff",
    "https://phoenix-gw.alibaba.com/api/adk/llm/generateContent?sg_k=fffffffffffffffffffffffffffffffe",
    "https://phoenix-gw.alibaba.com/api/adk/llm/generateContent?sg_k=00000000000000000000000000000000",
    "https://phoenix-gw.alibaba.com/api/adk/llm/generateContent?sg_k=ffffffffffffffffffffffffffffffff&extra=1",
  ];
  for (const u of qs) rec(factors(u, K));

  // Exp D: host flip
  rec({ exp: "D-host-flip", note: "www vs phoenix host" });
  rec(factors("https://phoenix-gw.alibaba.com/api/adk/llm/generateContent?sg_k=ffffffffffffffffffffffffffffffff", K));
  rec(factors("https://www.accio.com/api/adk/llm/generateContent?sg_k=ffffffffffffffffffffffffffffffff", K));

  // Exp E: getSecurityFactors component probe (other API)
  rec({ exp: "E-other-api", note: "getSecurityFactors raw output" });
  try {
    const r = addon.getSecurityFactors(JSON.stringify({ appkey: K, urlInput: U }));
    rec({ exp: "E", raw: String(r).slice(0, 800) });
  } catch (e) { rec({ exp: "E", err: String(e && e.message || e) }); }

  // Exp F: getExtraData probes
  for (const key of ["appkey", "sign", "wua", "umt", "umid", "token", "bx-version", "sgext"]) {
    try { rec({ exp: "F-getExtraData", key, val: String(addon.getExtraData(key)).slice(0, 200) }); }
    catch (e) { rec({ exp: "F-getExtraData", key, err: String(e && e.message || e) }); }
  }

  console.log("\n[dump] done ->", OUT);
}
main().catch((e) => { console.error("fatal:", e); process.exit(1); });
