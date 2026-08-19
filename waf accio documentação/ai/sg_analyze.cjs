"use strict";

// Offline analysis for JSONL captures produced by sg_dump.cjs / sg_fine.cjs.
// This script never calls the native SDK; it only characterizes captured output.

const fs = require("fs");
const path = require("path");

const defaults = [
  path.join(__dirname, "sg_dump.jsonl"),
  path.join(__dirname, "sg_fine.jsonl"),
];

function readRows(file) {
  if (!fs.existsSync(file)) return [];
  return fs.readFileSync(file, "utf8")
    .split(/\r?\n/)
    .filter(Boolean)
    .map((line) => ({ file: path.basename(file), ...JSON.parse(line) }));
}

function xorBitCount(a, b) {
  let count = 0;
  for (let i = 0; i < a.length; i++) {
    let value = a[i] ^ b[i];
    while (value !== 0) {
      count += value & 1;
      value >>>= 1;
    }
  }
  return count;
}

function byteDiffCount(a, b, start = 0) {
  let count = 0;
  for (let i = start; i < a.length; i++) {
    if (a[i] !== b[i]) count++;
  }
  return count;
}

function summarize(rows) {
  const factors = rows.filter((row) => typeof row["pctb-x-sign"] === "string");
  const marker = factors[0]?.["pctb-x-sign"].slice(0, 12);
  const blobs = factors.map((row) => Buffer.from(row["pctb-x-sign"].slice(12), "base64"));
  if (blobs.length === 0) {
    console.log("no factor records");
    return;
  }

  const tailRepeats = blobs.filter((blob) =>
    blob.subarray(-8, -4).equals(blob.subarray(-4)),
  ).length;

  const structuredTail = blobs.filter((blob) =>
    blob.length === 67 &&
    blob[1] === blob[13] &&
    blob[1] === blob[59] &&
    blob[1] === blob[61] &&
    blob[14] === blob[62],
  ).length;

  const maskedTailFields = blobs.filter((blob) =>
    blob.length === 67 &&
    ((blob[12] ^ blob[60]) & 0x0f) === 0 &&
    ((blob[12] ^ blob[14]) & 0x0f) === 0,
  ).length;

  console.log("records:", blobs.length);
  console.log("sign lengths:", [...new Set(factors.map((row) => row["pctb-x-sign"].length))].join(", "));
  console.log("marker:", marker);
  console.log("marker stable:", factors.every((row) => row["pctb-x-sign"].startsWith(marker)));
  console.log("payload Base64 lengths:", [...new Set(factors.map((row) => row["pctb-x-sign"].slice(12).length))].join(", "));
  console.log("decoded payload lengths:", [...new Set(blobs.map((blob) => blob.length))].join(", "));
  console.log("tail 4-byte repeat:", `${tailRepeats}/${blobs.length}`);
  console.log("payload positional tail layout:", `${structuredTail}/${blobs.length}`);
  console.log("tail field low-nibble mask:", `${maskedTailFields}/${blobs.length}`);
  console.log("tail examples:", blobs.slice(0, 8).map((blob) => blob.subarray(-8).toString("hex")).join(" "));

  const sameUrl = factors.filter((row) => row.url === factors[0].url);
  if (sameUrl.length >= 2) {
    const reference = Buffer.from(sameUrl[0]["pctb-x-sign"].slice(12), "base64");
    console.log("same-url reference:", sameUrl[0].url);
    for (let i = 1; i < sameUrl.length; i++) {
      const current = Buffer.from(sameUrl[i]["pctb-x-sign"].slice(12), "base64");
      console.log(`same-url pair ${i}:`, {
        changedPayloadBytes: byteDiffCount(reference, current),
        changedBits: xorBitCount(reference, current),
      });
    }
  }

  const byUrl = new Map();
  for (const row of factors) {
    byUrl.set(row.url, (byUrl.get(row.url) || 0) + 1);
  }
  console.log("distinct URLs:", byUrl.size);
  console.log("url multiplicities:", [...byUrl.values()].join(", "));
}

const files = process.argv.slice(2).length > 0 ? process.argv.slice(2) : defaults;
const rows = files.flatMap(readRows);
console.log("files:", files.join(", "));
summarize(rows);
