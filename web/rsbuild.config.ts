import { defineConfig } from "@rsbuild/core";
import { pluginReact } from "@rsbuild/plugin-react";
import { execSync } from "child_process";

/** Runs a git command and returns its trimmed output, or the fallback when it fails. */
function git(cmd: string, fallback: string): string {
  try {
    return execSync(cmd, { encoding: "utf8" }).trim();
  } catch {
    return fallback;
  }
}

const GIT_COMMIT  = git("git rev-parse --short=7 HEAD", "unknown");
const GIT_COUNT   = git("git rev-list --count HEAD", "0");
const GIT_BRANCH  = git("git rev-parse --abbrev-ref HEAD", "unknown");

export default defineConfig({
  plugins: [
    pluginReact(),
  ],
  source: {
    entry: { index: "./src/main.tsx" },
    define: {
      __GIT_COMMIT__: JSON.stringify(GIT_COMMIT),
      __GIT_COUNT__:  JSON.stringify(GIT_COUNT),
      __GIT_BRANCH__: JSON.stringify(GIT_BRANCH),
    },
  },
  server: {
    // Honor PORT so Playwright (and any other orchestrator) can spin the dev
    // server up on a freshly-discovered free port and avoid silently reusing
    // a stray unrelated process that happens to be bound to 3000. Default to
    // 3000 so plain `npm run dev` keeps working as before.
    port: process.env.PORT ? Number(process.env.PORT) : 3000,
    proxy: {
      "/ws": {
        target: "ws://localhost:3030",
        ws: true,
      },
    },
  },
  output: {
    distPath: { root: "dist" },
    sourceMap: { js: "source-map", css: false },
  },
  html: {
    title: "Rush",
  },
});
