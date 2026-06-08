/* 文件说明：vitest 测试 runner 配置。
   与 vite.config.ts 解耦（生产构建不依赖此文件，tsconfig 已 exclude *.test.ts，build 与 vitest 是否安装无关）。
   environment 'jsdom' 让组件/DOM 相关用例可跑；globals true 免去每个用例 import describe/it/expect；
   include 仅匹配 src 下的 *.test.{ts,tsx}，不扫生产源码。*/

import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  test: {
    environment: "jsdom",
    globals: true,
    include: ["src/**/*.test.{ts,tsx}"],
  },
});
