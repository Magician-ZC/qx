/* 文件说明：getMyCharacter / createMyCharacter 的 401 令牌失效处理聚焦测试（L1 修复回归）。
   锁定「401（token 失效/被登出）→ 镜像 getMe：先清本地 Bearer 再 rethrow」这一行为——修复前这两个函数
   遇 401 直接抛、不清 token，导致持过期 token 反复打 401、且与 getMe（已清）行为不一致。
   非 401 错误（如 500/网络）与成功路径都不得动 token。
   注意：本文件被 tsconfig.json 的 exclude 排除，不参与 `npm run build` 的 tsc --noEmit；仅 `npm test`（vitest run）执行。*/

import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import {
  APIError,
  createMyCharacter,
  getAccountToken,
  getMyCharacter,
  setAccountToken,
} from "./api";

// mockFetch 用给定 status + body 伪造一次后端响应（request() 会读 response.text() 再按 ok 分支）。
function mockFetch(status: number, body: unknown): void {
  const text = typeof body === "string" ? body : JSON.stringify(body);
  vi.stubGlobal(
    "fetch",
    vi.fn(async () =>
      new Response(text, {
        status,
        headers: { "Content-Type": "application/json" },
      }),
    ),
  );
}

describe("getMyCharacter / createMyCharacter 401 令牌失效处理（L1）", () => {
  beforeEach(() => {
    // 每例先放一枚有效 token，便于断言它在 401 后被清、在其它情形被保留。
    setAccountToken("tok-valid");
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    setAccountToken("");
  });

  it("getMyCharacter 遇 401 时先清本地 Bearer 再 rethrow（镜像 getMe）", async () => {
    mockFetch(401, { error: "token 失效" });
    await expect(getMyCharacter()).rejects.toBeInstanceOf(APIError);
    // 关键：401 后本地令牌被清空，避免持过期 token 反复打 401。
    expect(getAccountToken()).toBe("");
  });

  it("createMyCharacter 遇 401 时同样先清本地 Bearer 再 rethrow", async () => {
    mockFetch(401, { error: "token 失效" });
    await expect(createMyCharacter({ name: "她" })).rejects.toBeInstanceOf(APIError);
    expect(getAccountToken()).toBe("");
  });

  it("非 401 错误（如 500）不清 token——区别于会话失效", async () => {
    mockFetch(500, { error: "服务器开小差" });
    await expect(getMyCharacter()).rejects.toBeInstanceOf(APIError);
    // 500 是临时故障而非鉴权失效，令牌应保留以便重试。
    expect(getAccountToken()).toBe("tok-valid");
  });

  it("成功路径不动 token，且透传角色", async () => {
    mockFetch(200, { character: { has_character: true, session_id: "s1", unit_id: "u1", name: "她" } });
    const mine = await getMyCharacter();
    expect(mine.has_character).toBe(true);
    expect(mine.session_id).toBe("s1");
    expect(getAccountToken()).toBe("tok-valid");
  });
});
