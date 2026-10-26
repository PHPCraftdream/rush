import { defineConfig, devices } from '@playwright/test';
import { resolve } from 'node:path';
// CI-less invocation: build the web bundle first (`npm run build` in web/ so
// web/dist exists for the fixture's spaHandler), then run
// `npm run test:real-backend` (playwright test --config
// tests/real-backend/playwright.config.ts). The fixture main itself is not
// wired into CI workflows.
const repo=resolve(__dirname,'../../..');
export default defineConfig({testDir:'.',testMatch:'**/*.spec.ts',fullyParallel:false,retries:0,timeout:60_000,use:{baseURL:'http://127.0.0.1:60985',testIdAttribute:'data-test-id',...devices['Desktop Chrome'],trace:'retain-on-failure',screenshot:'only-on-failure'},webServer:{command:'go run ./internal/server/r15bfixture',cwd:repo,/* /auth/check always answers (200 or 401): a connection-level ready signal */ url:'http://127.0.0.1:60985/auth/check',timeout:180_000,reuseExistingServer:false,env:{RUSH_R15B_REAL_BACKEND:'1',RUSH_R15B_WEB_DIST:resolve(repo,'web/dist')}}});
