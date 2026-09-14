// Prove the TypeScript SDK re-exchanges credentials before the server expires
// them. Driven by scripts/refresh-check.sh; nightly only, since proving a
// refresh means outliving a credential.
import { readFileSync } from "node:fs";
import { PwrapClient } from "@pwrap/sdk";

const key = readFileSync("/tmp/refresh-key.txt", "utf8").trim();
const wait = Number(process.env.PWRAP_REFRESH_WAIT ?? "105");

const c = await PwrapClient.connect({ controlUrl: "http://localhost:8080", apiKey: key });
const first = c.expiresAt.getTime();
// Taken before any refresh on purpose: the swap has to reach handles that
// already exist, not only ones created afterwards.
const notes = c.table<{ n: number }>("refreshcheck");
await notes.insert({ n: 0 });

await new Promise((r) => setTimeout(r, wait * 1000));

try {
  await notes.insert({ n: 1 });
} catch (e) {
  console.error(`FAIL: insert after ${wait}s: ${(e as Error).message}`);
  process.exit(1);
}
if (c.expiresAt.getTime() <= first) {
  console.error(`FAIL: expiry never advanced from ${new Date(first).toISOString()}`);
  process.exit(1);
}
console.log(`  ts: PASS (expiry ${new Date(first).toISOString()} -> ${c.expiresAt.toISOString()})`);
await c.close();
process.exit(0);
