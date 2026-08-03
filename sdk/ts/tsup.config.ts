import { defineConfig } from "tsup";

export default defineConfig({
  entry: {
    index: "src/index.ts",
    neon: "src/neon.ts",
  },
  format: ["esm", "cjs"],
  dts: true,
  clean: true,
  sourcemap: true,
  target: "node18",
  splitting: false,
  // Block accidental bundling into a browser target — the SDK holds a DSN in memory
  // and has no business running in the browser. See risk #2 in the build plan.
  platform: "node",
});
