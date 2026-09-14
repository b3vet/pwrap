// Mirror the emitted .d.ts files as .d.cts for the `require` condition.
//
// package.json sets "type": "module", so TypeScript reads a .d.ts here as an ES
// module. A CommonJS consumer resolving the package under node16 then gets
// TS1479 ("cannot be imported with require") even though dist/index.cjs exists
// and works at runtime — the types disagree with the JS. Shipping .d.cts and
// pointing the require condition at it is what makes the two agree.
//
// Relative specifiers have to move with the extension: inside a .d.cts,
// "./client.js" would resolve back to the ESM declaration, so it becomes
// "./client.cjs" and resolves to client.d.cts alongside it.
import { readdir, readFile, writeFile } from "node:fs/promises";
import { join } from "node:path";

const dist = new URL("../dist/", import.meta.url).pathname;
const files = (await readdir(dist)).filter((f) => f.endsWith(".d.ts"));

if (files.length === 0) {
  console.error("dts-cjs: no .d.ts files in dist — did the declaration pass run?");
  process.exit(1);
}

for (const file of files) {
  const src = await readFile(join(dist, file), "utf8");
  // Only rewrite relative specifiers; bare package names are left alone.
  const out = src.replace(/(from\s+|import\()(["'])(\.\.?\/[^"']+)\.js\2/g, "$1$2$3.cjs$2");
  await writeFile(join(dist, file.replace(/\.d\.ts$/, ".d.cts")), out);
}
console.log(`dts-cjs: wrote ${files.length} .d.cts file(s)`);
