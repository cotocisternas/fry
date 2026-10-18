// Run only against make demo's disposable synthetic installation:
// FRY_DEMO_HTTP_PORT=8081 make demo
// FRY_DEMO_ORIGIN=http://127.0.0.1:8081 PLAYWRIGHT_MODULE=/path/to/playwright \
//   node scripts/browser/member-storage.cjs
// Requires an existing Playwright installation and Chromium; installs nothing.
// Real API calls check authorization. Browser routes supply deterministic edge
// cases without changing storage metadata or claiming runtime failure coverage.
'use strict';
const assert = require('node:assert/strict');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const origin = process.env.FRY_DEMO_ORIGIN || 'http://127.0.0.1:8081';
const target = new URL(origin);
if (target.hostname !== '127.0.0.1' || target.protocol !== 'http:' || target.pathname !== '/' || target.search || target.hash || target.username || target.password) {
  throw new Error('Requires a loopback disposable demo origin');
}
const member = '10000000000000000000000000000001';
const organization = '20000000000000000000000000000001';
const storageRoute = /\/fry\.admin\.v1\.AdminService\/GetMemberStorage$/;
const huge = Array.from({length: 19}, (_, i) => (9007199254740993n + BigInt(i)).toString());
const fileID = n => n.toString(16).padStart(32, '0');
const timestamp = '2026-10-18T12:00:00Z';
function snapshot() {
  const files = Array.from({length: 64}, (_, i) => ({file_id: fileID(i + 1), state: ['current', 'sealed', 'removed', 'building'][i % 4], charged_bytes: huge[15], sealed_records: i % 2 ? huge[16] : null, rewrite_job: i === 3 ? fileID(1000) : null}));
  return {
    member, sql_snapshot_at: timestamp,
    profile: {bounded: true, limits_frozen: true, max_message_bytes: huge[0], max_concurrent_appends: huge[1], max_bytes: huge[2], max_records: huge[3], max_file_bytes: huge[4], staging_reserved_bytes: huge[5], lane_version: huge[6], lane_count: '1'},
    current: {file_id: files[0].file_id, charged_bytes: huge[7], reserved_records: huge[8], reserved_record_bytes: huge[9], can_fit_max_record_snapshot: true, lanes: [{lane: '0', record_ceiling: huge[10], byte_ceiling: huge[11], reserved_records: huge[12], reserved_bytes: huge[13]}]},
    owner: {epoch: huge[14], closed: false}, files, next_file: files.at(-1).file_id,
    runtime: {status: 'sampled', checked_at: timestamp, sampled_at: '2026-10-18T12:00:01Z', member, failure_domain: 'synthetic-runtime', bound_owner_epoch: huge[17], owner_epoch: huge[18], current_file: files[1].file_id, closed: false, frozen: false, closure_uncertain: false}
  };
}
function unknownSnapshot(status) {
  const s = snapshot();
  s.profile.bounded = false; s.profile.limits_frozen = false;
  s.current.reserved_records = null; s.current.reserved_record_bytes = null; s.current.can_fit_max_record_snapshot = null;
  s.current.lanes = []; s.profile.lane_count = '0';
  s.owner = {epoch: null, closed: null};
  s.files = [{file_id: s.current.file_id, state: 'current', charged_bytes: huge[7], sealed_records: null, rewrite_job: null}];
  s.next_file = null;
  s.runtime = {status, checked_at: timestamp, sampled_at: null, member, failure_domain: 'synthetic-runtime', bound_owner_epoch: status === 'generation_mismatch' ? huge[17] : null, owner_epoch: status === 'generation_mismatch' ? huge[18] : null, current_file: null, closed: null, frozen: null, closure_uncertain: null};
  return s;
}
function deferred() { let resolve; const promise = new Promise(done => { resolve = done; }); return {promise, resolve}; }
async function bounded(promise, label) {
  let timer;
  try { return await Promise.race([promise, new Promise((_, reject) => { timer = setTimeout(() => reject(new Error('Timed out: ' + label)), 10000); })]); }
  finally { clearTimeout(timer); }
}
async function painted(page) { await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)))); }
async function login(page, username = 'operator', password = 'disposable-demo-admin') {
  await page.locator('#login-form input[name=username]').fill(username);
  await page.locator('#login-form input[name=password]').fill(password);
  const assigned = page.waitForResponse(r => new URL(r.url()).pathname === '/fry.admin.v1.AdminService/ListOrganizations' && r.request().method() === 'POST');
  await page.locator('#login-form button').click();
  const response = await assigned; assert.equal(response.status(), 200); await response.finished();
  await page.locator('#login').waitFor({state: 'hidden'});
  await page.locator('#organizations .selected').waitFor();
}
async function logout(page) {
  await page.locator('#logout').click(); await page.locator('#login').waitFor();
  assert.equal(await page.locator('#listing').innerText(), '');
  assert.equal(await page.locator('#member-storage-form input[name=member]').inputValue(), '');
}
async function inspect(page, value = member) {
  await page.locator('#member-storage-form input[name=member]').fill(value);
  await page.locator('#member-storage-form button').click();
}
async function ready(page) {
  await page.locator('#listing table').first().waitFor();
  await page.waitForFunction(() => !document.querySelector('#member-storage-form button').disabled);
}
function section(page, title) {
  return page.locator('#listing section').filter({has: page.getByRole('heading', {name: title, exact: true})});
}
async function rowValue(page, label, expected, title) {
  const scope = title ? section(page, title) : page.locator('#listing');
  const row = scope.locator('tr').filter({has: page.getByText(label, {exact: true})});
  assert.equal(await row.count(), 1, 'Expected one metric row: ' + label);
  assert.equal(await row.locator('td').last().innerText(), expected, label);
}
(async () => {
  const watchdog = setTimeout(() => { console.error('Member-storage UI regression exceeded 180 seconds'); process.exit(1); }, 180000);
  watchdog.unref();
  const browser = await chromium.launch({headless: true, executablePath: process.env.CHROMIUM_PATH || '/usr/bin/chromium'});
  try {
    const page = await browser.newPage(); page.setDefaultTimeout(10000); page.setDefaultNavigationTimeout(15000);
    const errors = []; page.on('pageerror', error => errors.push(error.message));
    await page.goto(origin); await login(page);
    // These calls bypass browser route mocks and use actual fixture sessions.
    const who = (await (await page.request.post(origin + '/fry.admin.v1.AdminService/GetSession', {data: {}, headers: {Origin: origin}})).json()).result;
    const real = await page.request.post(origin + '/fry.admin.v1.AdminService/GetMemberStorage', {data: {member, limit: 64}, headers: {Origin: origin, 'X-FRY-CSRF': who.csrf}});
    assert.equal(real.status(), 200);
    assert.equal((await real.json()).result.member, member);
    const actual = await page.evaluate(async member => {
      const identity = await FRYAdmin.call('session', undefined, '');
      return FRYAdmin.call('member-storage?' + new URLSearchParams({member, limit: '64'}), undefined, identity.csrf);
    }, member);
    assert.equal(actual.member, member);
    assert.equal(typeof actual.current.charged_bytes, 'string'); assert.ok(actual.files.length <= 64);
    await page.locator('[data-tab=memberstorage]').click();
    await inspect(page); await ready(page);
    assert.ok(await page.locator('#listing').getByText(actual.current.file_id, {exact: true}).count());

    async function post(path, data) {
      const response = await page.request.post(origin + '/fry.admin.v1.AdminService/' + path, {data, headers: {Origin: origin, 'X-FRY-CSRF': who.csrf}});
      assert.ok(response.ok(), path + ': ' + response.status() + ' ' + await response.text());
      const result = await response.json(); return result.result ?? result;
    }
    const managerName = 'storage-ui-manager-' + Date.now();
    const managerPassword = 'disposable-storage-ui-manager';
    const manager = await post('PutUser', {username: managerName, password: managerPassword, enabled: true});
    const secondOrg = await post('PutOrganization', {name: 'Member storage browser fixture ' + Date.now()});
    await post('ReplaceGrants', {user: manager.id, grants: [{scope: organization, content: true}, {scope: secondOrg.id, content: true}]});

    let reply = {body: snapshot(), status: 200}, hold = null;
    const queries = [];
    await page.route(storageRoute, async route => {
      const input = route.request().postDataJSON(); queries.push(new URLSearchParams(Object.entries(input).map(([key, value]) => [key, String(value)])));
      const response = reply, waiting = hold; hold = null;
      if (waiting) { waiting.entered.resolve(); await waiting.release.promise; }
      await route.fulfill({status: response.status, contentType: 'application/json', body: JSON.stringify(response.status === 200 ? {result: response.body} : {code: response.status === 401 ? 'unauthenticated' : 'not_found', message: response.body.error})});
    });
    async function refreshWith(body, status = 200) {
      reply = {body, status}; await page.locator('#refresh').click();
      if (status === 200) await ready(page); else await page.locator('#notice.error').waitFor();
    }
    function holdNext(body, status = 200) {
      reply = {body, status}; const waiting = {entered: deferred(), release: deferred()}; hold = waiting; return waiting;
    }
    async function release(waiting) {
      const response = page.waitForResponse(r => storageRoute.test(r.url()));
      waiting.release.resolve(); await (await response).finished(); await painted(page);
    }

    await refreshWith(snapshot());
    for (const value of huge) assert.ok(await page.locator('#listing').getByText(value, {exact: true}).count(), 'Lost decimal precision: ' + value);
    await rowValue(page, 'Frozen', 'No');
    await rowValue(page, 'Closure uncertain', 'No');
    await rowValue(page, 'Owner closed', 'No', 'Runtime observation');
    await rowValue(page, 'Can fit maximum record at snapshot', 'Yes');
    const text = await page.locator('#listing').innerText();
    assert.match(text, /metadata snapshot/i); assert.match(text, /runtime observation/i);
    assert.match(text, /reservation|accounting/i);
    assert.match(text, /does not promise that the next message will be accepted/i);
    assert.match(text, /no health or closure guarantee/i);
    const times = await page.evaluate(values => values.map(v => new Date(v).toLocaleString()), [timestamp, '2026-10-18T12:00:01Z']);
    await rowValue(page, 'Database observation', times[0], 'Metadata snapshot');
    await rowValue(page, 'Sampled at', times[1], 'Runtime observation');
    assert.equal(await section(page, 'File history').locator('tbody tr').count(), 64);
    await page.locator('#next').waitFor();
    assert.equal(queries.at(-1).get('limit'), '64'); assert.equal(queries.at(-1).get('after') || '', '');
    const later = snapshot(); later.files = [{file_id: fileID(65), state: 'removed', charged_bytes: huge[15], sealed_records: null, rewrite_job: null}]; later.next_file = null;
    reply = {body: later, status: 200};
    await page.locator('#next').click(); await ready(page);
    assert.equal(queries.at(-1).get('after'), fileID(64)); assert.equal(queries.at(-1).get('limit'), '64');
    assert.equal(await page.locator('#next').isHidden(), true);
    assert.ok(await page.locator('#listing').getByText(fileID(65), {exact: true}).count());
    assert.equal(await section(page, 'File history').locator('tbody tr').count(), 1);
    await refreshWith(snapshot()); assert.equal(queries.at(-1).get('after') || '', '');

    const observedFlags = snapshot();
    observedFlags.runtime.closed = observedFlags.runtime.frozen = observedFlags.runtime.closure_uncertain = true;
    observedFlags.current.can_fit_max_record_snapshot = false;
    await refreshWith(observedFlags);
    await rowValue(page, 'Frozen', 'Yes'); await rowValue(page, 'Closure uncertain', 'Yes');
    await rowValue(page, 'Owner closed', 'Yes', 'Runtime observation');
    await rowValue(page, 'Can fit maximum record at snapshot', 'No');
    const statuses = {
      gate_busy: 'Gate busy — guarded state unknown',
      unreachable: 'Unreachable — guarded state unknown',
      generation_mismatch: 'Owner generation mismatch — guarded state unknown',
      invalid_response: 'Invalid response — guarded state unknown'
    };
    for (const [status, label] of Object.entries(statuses)) {
      await refreshWith(unknownSnapshot(status));
      await rowValue(page, 'Status', label, 'Runtime observation');
      await rowValue(page, 'Frozen', 'Unknown'); await rowValue(page, 'Closure uncertain', 'Unknown');
      await rowValue(page, 'Owner closed', 'Unknown', 'Runtime observation');
      await rowValue(page, 'Can fit maximum record at snapshot', 'Unknown');
      await rowValue(page, 'Reserved records', 'Unknown', 'Current file reservations');
      await rowValue(page, 'Sampled at', 'Unknown', 'Runtime observation');
      assert.ok(await page.locator('#listing').getByText('Unknown', {exact: true}).count() >= 6, status + ' must preserve nullable fields');
      assert.match(await page.locator('#listing').innerText(), /legacy|not frozen|historical|not enforced/i);
      assert.equal(await page.locator('#next').isHidden(), true);
    }

    // Failed lookup removes prior diagnostics and leaves the input retryable.
    await refreshWith({error: 'Synthetic member lookup failed'}, 404);
    assert.match(await page.locator('#notice').innerText(), /Synthetic member lookup failed/);
    assert.equal(await page.locator('#listing table').count(), 0);
    assert.equal(await page.locator('#member-storage-form input[name=member]').isDisabled(), false);
    assert.equal(await page.locator('#member-storage-form button').isDisabled(), false);
    reply = {body: snapshot(), status: 200}; await inspect(page); await ready(page);

    const oldFile = 'eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee';
    function markedSnapshot() { const s = snapshot(); s.current.file_id = oldFile; s.files = [{...s.files[0], file_id: oldFile}]; s.next_file = null; return s; }
    let waiting = holdNext(markedSnapshot());
    await page.locator('#refresh').click(); await bounded(waiting.entered.promise, 'delayed tab request');
    await page.locator('[data-tab=inboxes]').click(); await page.locator('#listing table').waitFor();
    await release(waiting);
    assert.equal(await page.locator('#listing').getByText(oldFile, {exact: true}).count(), 0);
    assert.equal(await page.locator('#section-title').innerText(), 'Inboxes');

    reply = {body: snapshot(), status: 200}; await page.locator('[data-tab=memberstorage]').click();
    // Returning to the tab reloads the selected member.
    await ready(page);
    waiting = holdNext(markedSnapshot());
    await page.locator('#refresh').click(); await bounded(waiting.entered.promise, 'delayed signed-out request');
    // Hold the real logout response, including its expired session cookie.
    // Sensitive rows clear immediately, but login waits for cookie deletion.
    const logoutEntered = deferred(), logoutRelease = deferred();
    const logoutRoute = /\/fry\.admin\.v1\.AdminService\/Logout$/;
    await page.route(logoutRoute, async route => {
      const response = await route.fetch(); logoutEntered.resolve();
      await logoutRelease.promise; await route.fulfill({response});
    });
    await logout(page); await bounded(logoutEntered.promise, 'delayed logout');
    const loginControls = page.locator('#login-form input, #login-form button');
    for (let i = 0; i < await loginControls.count(); i++) assert.equal(await loginControls.nth(i).isDisabled(), true);
    const logoutDone = page.waitForResponse(r => logoutRoute.test(r.url()));
    logoutRelease.resolve(); await (await logoutDone).finished();
    await page.unroute(logoutRoute);
    await login(page);
    assert.equal((await page.request.post(origin + '/fry.admin.v1.AdminService/GetSession', {data: {}, headers: {Origin: origin}})).status(), 200, 'New login cookie survives completed logout');
    await release(waiting);
    assert.equal(await page.locator('#listing').getByText(oldFile, {exact: true}).count(), 0);
    assert.equal(await page.locator('#member-storage-form input[name=member]').inputValue(), '');

    await page.locator('[data-tab=memberstorage]').click();
    reply = {body: snapshot(), status: 200}; await inspect(page); await ready(page);
    waiting = holdNext({error: 'Synthetic expired operator session'}, 401);
    await page.locator('#refresh').click(); await bounded(waiting.entered.promise, 'delayed old-session 401');
    await logout(page); await login(page, managerName, managerPassword);
    const managerSession = (await (await page.request.post(origin + '/fry.admin.v1.AdminService/GetSession', {data: {}, headers: {Origin: origin}})).json()).result;
    const denied = await page.request.post(origin + '/fry.admin.v1.AdminService/GetMemberStorage', {data: {member, limit: 64}, headers: {Origin: origin, 'X-FRY-CSRF': managerSession.csrf}});
    assert.equal(denied.status(), 403, 'Multiple organization/content grants cannot inspect members');
    assert.equal(await page.locator('#member-storage-tab').isHidden(), true);
    assert.equal(await page.locator('#member-storage-form').isHidden(), true);
    assert.equal(await page.locator('#section-title').innerText(), 'Inboxes');
    await release(waiting);
    assert.equal(await page.locator('#login').isHidden(), true, 'Old-session 401 must not sign the manager out');
    assert.equal(await page.locator('#identity').innerText(), managerName);
    assert.equal(await page.locator('#member-storage-tab').isHidden(), true);
    assert.equal(await page.locator('#listing').getByText(oldFile, {exact: true}).count(), 0);
    assert.doesNotMatch(await page.locator('#notice').innerText(), /Synthetic expired operator session/);
    await page.locator('#refresh').click(); await page.locator('#listing table').waitFor();
    assert.equal(await page.locator('#member-storage-form input[name=member]').inputValue(), '');
    assert.ok(queries.every(query => query.get('limit') === '64' && query.get('member') === member));
    await logout(page); assert.deepEqual(errors, []);
    console.log('PASS: real operator/manager authorization, exact counters, unknown runtime state, bounded pagination/reset, lookup retry, late tab/session responses and manager isolation');
  } finally { await browser.close(); clearTimeout(watchdog); }
})().catch(error => { console.error(error); process.exitCode = 1; });
