import { join } from "node:path";
import { pathToFileURL } from "node:url";

// Keep the bridge protocol on stdout machine-readable. The native package logs
// diagnostics to console; route those diagnostics to stderr instead.
console.log = (...args) => process.stderr.write(args.map(String).join(" ") + "\n");
console.warn = (...args) => process.stderr.write(args.map(String).join(" ") + "\n");
console.error = (...args) => process.stderr.write(args.map(String).join(" ") + "\n");

const url = process.env.ACCIO_SECURITY_URL || "";
const packageDir = process.env.ACCIO_SECURITY_GUARD_DIR || "";

try {
  if (!url || !packageDir) throw new Error("security guard bridge requires url and package directory");
  const mod = await import(pathToFileURL(join(packageDir, "dist", "index.js")).href);
  try {
    await mod.initUmid(6);
  } catch (error) {
    // The native SDK is process-global and may already be initialized by the
    // Accio desktop process. That condition is safe to continue past.
    if (!/errorCode=1\b/i.test(String(error?.message || error))) throw error;
  }
  const result = mod.getSecurityFactorsForWebHeaders({ url });
  if (!result?.headers || typeof result.headers !== "object") {
    throw new Error(result?.errInfo || result?.errType || "security guard returned no headers");
  }
  process.stdout.write(JSON.stringify({ headers: result.headers }));
} catch (error) {
  process.stdout.write(JSON.stringify({ error: String(error?.message || error) }));
  process.exitCode = 1;
}
