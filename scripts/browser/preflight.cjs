'use strict';
const assert = require('node:assert/strict');
const { chromium } = require('playwright');
const expected = require('./package.json').devDependencies.playwright;
assert.equal(require('playwright/package.json').version, expected, 'Run scripts/browser/setup.sh');
assert.equal(require('playwright-core/package.json').version, expected, 'Run scripts/browser/setup.sh');
(async () => {
  const browser = await chromium.launch({headless: true});
  try {
    console.log('Chromium ' + browser.version() + ' / Playwright ' + expected);
  } finally { await browser.close(); }
})().catch(() => {
  console.error('Pinned Chromium cannot start; run scripts/browser/setup.sh (CI: --with-deps).');
  process.exitCode = 1;
});
