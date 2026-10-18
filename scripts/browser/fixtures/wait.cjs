// Hold a real Chromium process while lifecycle_test.py interrupts its owner.
'use strict';
const fs = require('node:fs');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE);
(async () => {
  await chromium.launch({headless: true});
  fs.writeFileSync(process.env.FRY_BROWSER_READY_FILE, String(process.pid));
  setInterval(() => {}, 1000);
})().catch(() => { process.exitCode = 1; });
