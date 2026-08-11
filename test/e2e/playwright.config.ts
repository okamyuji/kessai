import { defineConfig, devices } from "@playwright/test";

// kessai E2E設定
// - 起動: 実際のGoサーバを起動する（package.json scripts.test-server を webServer に指定）
// - 対象ブラウザ: Chromium 1系のみ。決済動作の網羅を優先し、複数ブラウザ検証は後段。
// - baseURL: HTTPで localhost:8080。開発時はKESSAI_INSECURE_COOKIE=1でCookieのSecureを外す。
export default defineConfig({
  testDir: ".",
  timeout: 60_000,
  reporter: [["list"]],
  use: {
    baseURL: process.env.KESSAI_BASE_URL || "http://127.0.0.1:8080",
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
});
