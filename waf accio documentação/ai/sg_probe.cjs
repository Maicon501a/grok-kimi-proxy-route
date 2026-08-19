"use strict";
// Probe the Accio SecurityGuard native addon: dump WAF headers for sample URLs.
const path = require("path");

const addonPath = process.env.SG_ADDON || "";
const addon = require(addonPath);

console.log("[probe] addon keys:", Object.keys(addon));
console.log("[probe] isStub:", addon.isStub);

async function run() {
  // initUmid(6) = AREA_ONLINE
  try {
    const token = await addon.initUmid(6);
    console.log("[probe] initUmid(6) ok, token:", typeof token === "string" ? token.slice(0, 40) + "..." : token);
  } catch (e) {
    console.log("[probe] initUmid(6) err:", String(e && e.message || e));
  }

  const samples = [
    "https://phoenix-gw.alibaba.com/api/adk/llm/generateContent?sg_k=f0b7c055f59eda6c8a006088227277b7",
    "https://phoenix-gw.alibaba.com/api/rc/pc/token?sg_k=md5ofreqtimestamp",
    "https://www.accio.com/api/user/login",
    "https://phoenix-gw.alibaba.com/api/tool/featureFlag/evaluate"
  ];

  for (const url of samples) {
    try {
      const input = JSON.stringify({ appkey: "35336201", urlInput: url });
      const raw = addon.getSecurityFactorsForWeb(input);
      console.log("\n[probe] URL:", url);
      console.log("[probe] raw:", raw);
      try {
        const parsed = JSON.parse(raw);
        console.log("[probe] header names:", Object.keys(parsed).join(", "));
      } catch { }
    } catch (e) {
      console.log("\n[probe] URL:", url, "ERR:", String(e && e.message || e));
    }
  }

  // extraData / urlSign attempts
  try {
    const ed = addon.getExtraData("appkey");
    console.log("\n[probe] getExtraData(appkey):", ed);
  } catch (e) {
    console.log("[probe] getExtraData err:", String(e && e.message || e));
  }
  try {
    const tok = addon.getSecurityToken(6);
    console.log("[probe] getSecurityToken(6):", tok);
  } catch (e) {
    console.log("[probe] getSecurityToken err:", String(e && e.message || e));
  }
  try {
    const sign = addon.urlSign("35336201", "test");
    console.log("[probe] urlSign:", sign);
  } catch (e) {
    console.log("[probe] urlSign err:", String(e && e.message || e));
  }
}

run().catch((e) => { console.error("[probe] fatal:", e); process.exit(1); });
