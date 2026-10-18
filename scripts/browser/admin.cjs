// Run only against make demo's disposable synthetic installation.
// Supply PLAYWRIGHT_MODULE when Playwright is installed outside this repository.
'use strict';
const assert = require('node:assert/strict');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const origin = process.env.FRY_DEMO_ORIGIN || 'http://127.0.0.1:8081';
if (new URL(origin).hostname !== '127.0.0.1') throw new Error('Requires a loopback disposable demo');
(async () => {
  const browser = await chromium.launch({headless: true, executablePath: process.env.CHROMIUM_PATH || '/usr/bin/chromium'});
  try {
    const page = await browser.newPage();
    const errors = []; page.on('pageerror', error => errors.push(error.message));
    await page.goto(origin);
    await page.locator('#login-form input[name=username]').fill('operator');
    await page.locator('#login-form input[name=password]').fill('disposable-demo-admin');
    await page.locator('#login-form button').click();
    await page.locator('#listing table').waitFor();
    const firstOrg = await page.locator('#organizations .selected small').innerText();
    const firstInbox = await page.locator('#listing tbody tr td').first().innerText();
    const secondName = 'UI check ' + Date.now();
    await page.locator('#organization-form input').fill(secondName);
    await page.locator('#organization-form button').click();
    await page.locator('#organizations .selected').filter({hasText: secondName}).waitFor();
    await page.locator('#inbox-form button').click();
    await page.locator('#listing table').waitFor();
    const secondOrg = await page.locator('#organizations .selected small').innerText();
    const secondInbox = await page.locator('#listing tbody tr td').first().innerText();
    assert.notEqual(firstOrg, secondOrg); assert.notEqual(firstInbox, secondInbox);

    let unblock, requested;
    const held = new Promise(resolve => { unblock = resolve; });
    const entered = new Promise(resolve => { requested = resolve; });
    await page.route('**/fry.admin.v1.AdminService/ListInboxes', async route => {
      if (route.request().postDataJSON().organization !== firstOrg) { await route.continue(); return; }
      const response = await route.fetch(); requested(); await held; await route.fulfill({response});
    });
    await page.locator('#organizations button').filter({hasText:firstOrg}).click();
    await entered;
    assert.equal(await page.locator('#inbox-form button').isDisabled(), true);
    await page.locator('#organizations button').filter({hasText:secondOrg}).click();
    await page.locator('#listing').getByText(secondInbox, {exact:true}).waitFor();
    const responseDone = page.waitForResponse(r => r.url().endsWith('/fry.admin.v1.AdminService/ListInboxes') && r.request().postDataJSON().organization === firstOrg);
    unblock(); await responseDone;
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    assert.equal(await page.locator('#listing').getByText(firstInbox, {exact:true}).count(), 0);
    assert.equal(await page.locator('#listing').getByText(secondInbox, {exact:true}).count(), 1);
    await page.unroute('**/fry.admin.v1.AdminService/ListInboxes');

    await page.locator('[data-tab=mailaccounts]').click();
    await page.locator('#mail-account-form input[name=username]').fill('ui-fixture-' + Date.now());
    await page.locator('#mail-account-form input[name=password]').fill('disposable-ui-mail-password');
    await page.locator('#mail-account-form button').click();
    await page.locator('#listing table').waitFor();
    assert.match(await page.locator('#listing').innerText(), /Enabled/);
    await page.locator('#listing button').filter({hasText:'Disable'}).click();
    await page.locator('#listing button').filter({hasText:'Enable'}).waitFor();
    assert.match(await page.locator('#listing').innerText(), /Disabled/);
    await page.locator('[data-tab=senders]').click();
    await page.locator('#sender-challenge-form input[name=scope]').fill('sender-ui.example.test');
    await page.locator('#sender-challenge-form button').click();
    await page.locator('#sender-proof').waitFor();
    assert.equal(await page.locator('#sender-dns-name').inputValue(), '_fry-verify.sender-ui.example.test.');
    assert.match(await page.locator('#sender-dns-value').inputValue(), /^fry-sender-v1=[0-9a-f]{64}$/);
    assert.match(await page.locator('#sender-verify-form input').inputValue(), /^[0-9a-f]{32}$/);
    await page.locator('#sender-override-form input[name=scope]').fill('sender-ui.example.test');
    await page.locator('#sender-override-form input[name=reason]').fill('Synthetic browser verification of audited operator override');
    await page.locator('#sender-override-form button').click();
    await page.locator('#listing button').filter({hasText:'Revoke'}).waitFor();
    assert.match(await page.locator('#listing').innerText(), /Authorized/);
    assert.match(await page.locator('#listing').innerText(), /Synthetic browser verification/);
    assert.equal(await page.locator('#sender-proof').isHidden(), true);
    await page.locator('#listing button').filter({hasText:'Revoke'}).click();
    await page.getByText('Inactive', {exact:true}).waitFor();

    // A delayed challenge response must not expose another organization's
    // record or populate the currently selected organization's verification form.
    let releaseChallenge, challengeRequested;
    const challengeHeld = new Promise(resolve => { releaseChallenge = resolve; });
    const challengeEntered = new Promise(resolve => { challengeRequested = resolve; });
    await page.route('**/fry.admin.v1.AdminService/CreateSenderChallenge', async route => {
      const response = await route.fetch(); challengeRequested(); await challengeHeld; await route.fulfill({response});
    });
    await page.locator('#sender-challenge-form input[name=scope]').fill('delayed-ui.example.test');
    await page.locator('#sender-challenge-form button').click();
    await challengeEntered;
    await page.locator('#organizations button').filter({hasText:firstOrg}).click();
    await page.locator('#organizations .selected').filter({hasText:firstOrg}).waitFor();
    const challengeDone = page.waitForResponse(r => r.url().endsWith('/fry.admin.v1.AdminService/CreateSenderChallenge'));
    releaseChallenge(); await challengeDone;
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    assert.equal(await page.locator('#sender-proof').isHidden(), true);
    assert.equal(await page.locator('#sender-verify-form input').inputValue(), '');
    await page.unroute('**/fry.admin.v1.AdminService/CreateSenderChallenge');
    await page.locator('#logout').click();
    await page.locator('#login').waitFor();
    assert.equal(await page.locator('#listing').innerText(), '');
    assert.deepEqual(errors, []);
    console.log('PASS: organization response ordering, credential creation/revocation, sender challenge/override/revocation and response isolation, logout cleanup');
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
