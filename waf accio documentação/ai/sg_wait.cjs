"use strict";

const addon = require(process.env.SG_ADDON || "");

(async () => {
  await addon.initUmid(6);
  const input = JSON.stringify({
    appkey: process.env.SG_APPKEY || "35336201",
    urlInput: "https://phoenix-gw.alibaba.com/api/adk/llm/generateContent?sg_k=ffffffffffffffffffffffffffffffff",
  });
  const calls = Math.max(1, Number(process.env.SG_CALLS || "1"));
  for (let i = 0; i < calls; i++) {
    const raw = addon.getSecurityFactorsForWeb(input);
    process.stdout.write(`READY ${process.pid} ${i + 1} ${raw.length}\n`);
  }
  setInterval(() => {}, 1000);
})().catch((error) => {
  process.stderr.write(String(error && error.stack || error) + "\n");
  process.exit(1);
});
