import { defineConfig } from "tsup";

export default defineConfig({
  entry: {
    index: "src/index.ts",
    neon: "src/neon.ts",
  },
  format: ["esm", "cjs"],
  // Declarations come from `tsc -p tsconfig.build.json` instead: tsup's bundled
  // rollup-plugin-dts is pinned at a version that breaks on newer TypeScript
  // and cannot be overridden, because tsup inlines it rather than depending
  // on it. See tsconfig.build.json.
  dts: false,
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
  external: ["@neondatabase/serverless", "ws"],
});
