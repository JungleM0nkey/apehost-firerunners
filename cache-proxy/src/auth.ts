import type { Env } from "./env";

export type Scope = "rw" | "ro";

interface TokenSet {
  key: string;
  digests: { digest: ArrayBuffer; scope: Scope }[];
}

let cached: TokenSet | null = null;
const encoder = new TextEncoder();
const sha256 = (s: string) => crypto.subtle.digest("SHA-256", encoder.encode(s));

const split = (list: string | null | undefined): string[] =>
  (list ?? "")
    .split(",")
    .map((t) => t.trim())
    .filter((t) => t.length > 0);

async function tokenSet(env: Env): Promise<TokenSet> {
  const key = `${env.TOKENS_RW ?? ""}\n${env.TOKENS_RO ?? ""}`;
  if (cached?.key === key) return cached;
  const digests: TokenSet["digests"] = [];
  for (const t of split(env.TOKENS_RW)) digests.push({ digest: await sha256(t), scope: "rw" });
  for (const t of split(env.TOKENS_RO)) digests.push({ digest: await sha256(t), scope: "ro" });
  cached = { key, digests };
  return cached;
}

/**
 * Resolve the bearer token's scope. Both sides are hashed to fixed-length digests and every
 * configured token is compared with timingSafeEqual (no early exit), so neither the position
 * of a match nor a common prefix shows up in timing.
 */
export async function authenticate(request: Request, env: Env): Promise<Scope | null> {
  const m = /^Bearer\s+(\S+)\s*$/i.exec(request.headers.get("authorization") ?? "");
  const presented = await sha256(m?.[1] ?? "");
  let scope: Scope | null = null;
  for (const t of (await tokenSet(env)).digests) {
    const equal = crypto.subtle.timingSafeEqual(presented, t.digest);
    if (equal && m && scope !== "rw") scope = t.scope;
  }
  return scope;
}
