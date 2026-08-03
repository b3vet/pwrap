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
  // tsup externalises `dependencies` and `peerDependencies` automatically, but not
  // a dynamic import of an optional peer. Without this, the whole Neon serverless
  // driver gets inlined into dist/neon.js — which ships a copy to every consumer,
  // makes the "not installed" error unreachable, and risks two live copies for
  // anyone who installs it themselves.
  external: ["@neondatabase/serverless"],
});
