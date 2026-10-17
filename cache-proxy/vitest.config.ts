import { cloudflareTest } from "@cloudflare/vitest-plugin";
import { defineConfig } from "vitest/config";
import { createFakeDisk, createFakeUpstream, UPSTREAM_TOKEN } from "./test/node/fakes.ts";

export function cacheTestConfig(upstream: string | null) {
  return defineConfig({
    plugins: [
      cloudflareTest({
        main: "./src/index.ts",
        miniflare: {
          compatibilityDate: "2026-10-01",
          compatibilityFlags: ["nodejs_compat"], // required by the test runner only, not by the Worker
          durableObjects: { INDEX: { className: "CacheIndex", useSQLite: true } },
          serviceBindings: { DISK: createFakeDisk(), UPSTREAM: createFakeUpstream() },
          bindings: {
            SETTINGS: { upstream, maxBytes: 1000, maintenanceIntervalSeconds: 3600 },
            UPSTREAM_TOKEN,
            TOKENS_RW: "rw-token, rw-token-2",
            TOKENS_RO: "ro-token",
          },
        },
      }),
    ],
    // The fake services keep global state (failure toggles), so test files run one at a time.
    test: {
      include: upstream === null ? ["test/local-only.test.ts"] : ["test/**/*.test.ts"],
      exclude: upstream === null ? [] : ["test/local-only.test.ts"],
      fileParallelism: false,
    },
  });
}

export default cacheTestConfig("http://upstream.test");
