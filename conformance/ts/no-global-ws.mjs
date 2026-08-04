// Removes the global WebSocket so the runner exercises the `ws` fallback path —
// i.e. what happens on Node 18 and 20, where no global exists. Used by
// `pnpm start:no-global-ws` so that path is testable on any Node version.
delete globalThis.WebSocket;
