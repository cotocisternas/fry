'use strict';
const $ = (id) => document.getElementById(id);
let viewGeneration = 0;
let senderChallenge = null;
let storageMember = '', storageAfter = '';
let signingOut = false;
const staleView = Symbol('stale view');
let identity, organizations = [], users = [], organization = '', tab = 'inboxes', after = '', before = 0;
function notice(message = '', error = false) { $('notice').textContent = message; $('notice').classList.toggle('error', error); }
async function api(path, data) {
  const requestIdentity = identity;
  try { return await FRYAdmin.call(path, data, identity?.csrf || ''); }
  catch (error) {
    if (FRYAdmin.unauthenticated(error) && path !== 'login' && identity === requestIdentity) signedOut();
    throw error;
  }
}
function node(tag, text, className) { const n = document.createElement(tag); if (text !== undefined) n.textContent = text; if (className) n.className = className; return n; }
function button(text, action) { const b = node('button', text); b.type = 'button'; b.addEventListener('click', () => task(action)); return b; }
async function task(action) { notice(); try { await action(); } catch (e) { if (e !== staleView) notice(e.message, true); } }
function signedOut() { viewGeneration++; senderChallenge = null; storageMember = storageAfter = ''; $('member-storage-tab').hidden = $('member-storage-form').hidden = true; $('sender-proof').hidden = true; identity = null; users = []; organizations = []; organization = ''; $('listing').replaceChildren(); document.querySelectorAll('form').forEach(f => f.reset()); $('login').hidden = false; $('workspace').hidden = true; $('logout').hidden = true; $('identity').textContent = ''; }
function globalOperator() { return identity.grants.some(g => g.scope === '*'); }
function canRead() { return identity.grants.some(g => (g.scope === '*' || g.scope === organization) && g.content); }
async function signedIn(who) {
  identity = who; $('login').hidden = true; $('workspace').hidden = false; $('logout').hidden = false; $('identity').textContent = who.name;
  $('organization-form').hidden = !globalOperator(); $('users-tab').hidden = $('member-storage-tab').hidden = !globalOperator();
  const assigned = await api('organizations');
  if (identity !== who) return;
  organizations = assigned;
  if (['users', 'memberstorage'].includes(tab) && !globalOperator()) tab = 'inboxes';
  if (!organizations.some(o => o.id === organization)) organization = organizations[0]?.id || '';
  renderOrganizations(); await show();
}
function renderOrganizations() {
  $('organizations').replaceChildren();
  for (const org of organizations) {
    const b = button(org.name, async () => { for (const id of ['domain-form', 'alias-form', 'mail-account-form', 'storage-quota-form', 'submission-create-form', 'submission-policy-form', 'submission-password-form', 'sender-challenge-form', 'sender-verify-form', 'sender-override-form']) $(id).reset(); organization = org.id; after = ''; before = 0; renderOrganizations(); await show(); });
    b.className = 'org' + (org.id === organization ? ' selected' : ''); b.append(node('small', org.id)); $('organizations').append(b);
  }
  if (!organizations.length) $('organizations').append(node('p', 'No assigned organizations.', 'muted'));
}
function table(headers, rows, target = $('listing')) {
  if (!rows.length) { target.replaceChildren(node('p', 'No records to show.', 'empty')); return; }
  const wrapper = node('div', undefined, 'table-wrap'), t = node('table'), head = node('tr');
  for (const h of headers) head.append(node('th', h));
  const thead = node('thead'); thead.append(head); t.append(thead); const body = node('tbody');
  for (const row of rows) { const tr = node('tr'); for (const item of row) { const td = node('td'); td.append(item instanceof Node ? item : document.createTextNode(String(item))); tr.append(td); } body.append(tr); }
  t.append(body); wrapper.append(t); target.replaceChildren(wrapper);
}
function memberStorage(snapshot) {
  const content = node('div', undefined, 'member-storage');
  const value = v => v === null || v === undefined ? 'Unknown' : v;
  const flag = v => v === true ? 'Yes' : v === false ? 'No' : 'Unknown';
  const time = v => v ? new Date(v).toLocaleString() : 'Unknown';
  const section = (title, description, headers, rows) => {
    const part = node('section'); part.append(node('h3', title));
    if (description) part.append(node('p', description, 'muted'));
    const target = node('div'); table(headers, rows, target); part.append(target); content.append(part);
  };
  section('Metadata snapshot', 'Reservations and charges describe this database observation. They are not filesystem usage, free space, S3 use or organization totals.', ['Field', 'Value'], [
    ['Member ID', node('span', snapshot.member, 'mono')], ['Database observation', time(snapshot.sql_snapshot_at)]
  ]);
  const p = snapshot.profile, c = snapshot.current, o = snapshot.owner, r = snapshot.runtime;
  section('Configured profile', p.bounded ? 'Bounded profile limits and staging reservations. Staging headroom is reserved capacity, not measured occupied blocks.' : 'Legacy unbounded profile: these historical registration values are not enforced bounded-profile guarantees.', ['Setting', 'Value'], [
    ['Bounded profile', flag(p.bounded)], ['Limits frozen', flag(p.limits_frozen)], ['Maximum message bytes', value(p.max_message_bytes)],
    ['Maximum concurrent appends', value(p.max_concurrent_appends)], ['Maximum member bytes', value(p.max_bytes)], ['Maximum file records', value(p.max_records)],
    ['Maximum file bytes', value(p.max_file_bytes)], ['Staging reserved bytes', value(p.staging_reserved_bytes)], ['Lane version', value(p.lane_version)], ['Lane count', value(p.lane_count)]
  ]);
  section('Current file reservations', 'Lane fit applies only to the database snapshot. It does not promise that the next message will be accepted.', ['Field', 'Value'], [
    ['Current file ID', node('span', c.file_id, 'mono')], ['Charged bytes', value(c.charged_bytes)], ['Reserved records', value(c.reserved_records)],
    ['Reserved record bytes', value(c.reserved_record_bytes)], ['Can fit maximum record at snapshot', flag(c.can_fit_max_record_snapshot)]
  ]);
  section('Capacity lanes', 'Per-lane ceilings and conservative reservations for the current file.', ['Lane', 'Record ceiling', 'Byte ceiling', 'Reserved records', 'Reserved bytes'],
    (c.lanes || []).map(l => [value(l.lane), value(l.record_ceiling), value(l.byte_ceiling), value(l.reserved_records), value(l.reserved_bytes)]));
  section('Latest metadata owner', 'An unclosed owner row records conservative closure proof debt. It is not a process liveness check.', ['Field', 'Value'], [
    ['Owner epoch', value(o.epoch)], ['Owner closed', flag(o.closed)]
  ]);
  const statuses = {sampled: 'Sample captured', gate_busy: 'Gate busy — guarded state unknown', unreachable: 'Unreachable — guarded state unknown', generation_mismatch: 'Owner generation mismatch — guarded state unknown', invalid_response: 'Invalid response — guarded state unknown'};
  const sampled = r.status === 'sampled';
  section('Runtime observation', 'A separate, later owner observation. Busy, unavailable or mismatched owners provide no health or closure guarantee. Inspection does not authorize admission, takeover or cleanup.', ['Field', 'Value'], [
    ['Status', statuses[r.status] || 'Unknown runtime status: ' + value(r.status)], ['Checked at', time(r.checked_at)], ['Sampled at', sampled ? time(r.sampled_at) : 'Unknown'],
    ['Member ID', node('span', value(r.member), 'mono')], ['Failure domain', value(r.failure_domain)], ['Bound owner epoch', value(r.bound_owner_epoch)], ['Observed owner epoch', value(r.owner_epoch)],
    ['Current file ID', node('span', sampled ? value(r.current_file) : 'Unknown', 'mono')], ['Owner closed', sampled ? flag(r.closed) : 'Unknown'],
    ['Frozen', sampled ? flag(r.frozen) : 'Unknown'], ['Closure uncertain', sampled ? flag(r.closure_uncertain) : 'Unknown']
  ]);
  if (sampled && r.current_file && r.current_file !== c.file_id) content.append(node('p', 'The runtime and metadata current files differ. The observations were taken at different times; rotation may have occurred.', 'muted'));
  section('File history', 'At most 64 catalog rows per page. Pages may observe concurrent changes; this is not a stable inventory. Charges are accounting obligations, and summing this page does not give a member or installation total. Refresh restarts at the first page.', ['File ID', 'State', 'Charged bytes', 'Sealed records', 'Rewrite job'],
    (snapshot.files || []).map(f => [node('span', f.file_id, 'mono'), f.state, value(f.charged_bytes), value(f.sealed_records), node('span', f.rewrite_job ?? 'None', 'mono')]));
  $('listing').replaceChildren(content);
}
async function show(viewOrganization = organization, viewTab = tab) {
  if (!identity) return;
  const organization = viewOrganization, tab = viewTab, generation = ++viewGeneration;
  const active = () => identity && generation === viewGeneration;
  const forms = ['inbox-form', 'domain-form', 'alias-form', 'mail-account-form', 'storage-quota-form', 'member-storage-form', 'submission-create-form', 'submission-policy-form', 'submission-password-form', 'user-form', 'grant-form', 'sender-challenge-form', 'sender-verify-form', 'sender-override-form'];
  const loading = value => forms.forEach(id => $(id).querySelectorAll('input, textarea, select, button').forEach(el => el.disabled = value));
  const load = async (path, data) => { if (!active()) throw staleView; try { const result = await api(path, data); if (!active()) throw staleView; return result; } catch (e) { if (!active()) throw staleView; throw e; } };
  let loaded = false;
  loading(true);
  $('listing').replaceChildren(node('p', 'Loading…', 'empty'));
  try {
    document.querySelectorAll('[data-tab]').forEach(b => b.classList.toggle('selected', b.dataset.tab === tab));
    $('storage-quota-form').hidden = tab !== 'storage' || !organization;
    $('member-storage-form').hidden = tab !== 'memberstorage' || !globalOperator();
    $('inbox-form').hidden = tab !== 'inboxes' || !organization;
    $('user-form').hidden = $('grant-form').hidden = tab !== 'users';
    $('domain-form').hidden = tab !== 'domains' || !organization; $('alias-form').hidden = tab !== 'aliases' || !organization; $('mail-account-form').hidden = tab !== 'mailaccounts' || !organization || !canRead(); $('next').hidden = true;
    for (const id of ['submission-create-form', 'submission-policy-form', 'submission-password-form']) $(id).hidden = tab !== 'submission' || !organization;
    $('sender-challenge-form').hidden = $('sender-verify-form').hidden = tab !== 'senders' || !organization;
    $('sender-override-form').hidden = tab !== 'senders' || !organization || !globalOperator();
    $('sender-proof').hidden = tab !== 'senders' || senderChallenge?.organization !== organization;
    if (!$('sender-proof').hidden) {
      $('sender-dns-name').value = senderChallenge.dns_name; $('sender-dns-value').value = senderChallenge.txt_value;
      $('sender-proof-expiry').textContent = 'Challenge expires ' + new Date(senderChallenge.expires_at).toLocaleString() + '. Copy this record before leaving this page.';
      $('sender-verify-form').elements.challenge.value = senderChallenge.id;
    }
    $('section-title').textContent = {inboxes: 'Inboxes', domains: 'Incoming domains', aliases: 'Incoming addresses', mailaccounts: 'Mail credentials', storage: 'Storage', memberstorage: 'Member storage', submission: 'SMTP credentials', senders: 'Outgoing senders', messages: 'Received messages', audit: 'Audit log', users: 'Administrators'}[tab];
    $('section-description').textContent = {inboxes: 'Shared mailbox state for the selected organization.', domains: 'Each domain belongs to one organization. Changes take effect for new recipients.', aliases: 'Route one address to one or several inboxes within this organization.', mailaccounts: 'Application credentials for conventional IMAP access.', storage: 'Logical retained originals and advisory alerts. Current usage and the last background alert sample are shown separately.', memberstorage: 'Read-only shared member diagnostics. Metadata reservations and runtime observations have separate times and meanings.', submission: 'Create send-only credentials and manage their permitted senders, passwords and access. Outgoing sender authorization is managed separately.', senders: 'Verified or explicitly overridden sender permissions for this organization. Revocation blocks new acceptance; already accepted mail remains queued.', messages: 'Original messages, including retained recovery copies. Downloads are audited.', audit: 'Configuration changes and complete message access.', users: 'Local accounts and explicitly assigned access.'}[tab];
    if (!organization && ['inboxes', 'domains', 'aliases', 'mailaccounts', 'storage', 'submission', 'senders', 'messages'].includes(tab)) { $('listing').replaceChildren(node('p', 'Select an organization to continue.', 'empty')); return; }
    if (tab === 'inboxes') { const rows = await load('inboxes?organization=' + organization); table(['Inbox ID', 'Visible messages'], rows.map(i => [node('span', i.id, 'mono'), i.messages])); }
    if (['domains', 'aliases', 'mailaccounts'].includes(tab)) {
      const inboxes = await load('inboxes?organization=' + organization);
      const options = () => inboxes.map(i => { const o = node('option', i.id); o.value = i.id; return o; });
      if (tab === 'domains') {
        const select = $('domain-form').elements.catch_all; const reject = node('option', 'Reject unknown recipients'); reject.value = ''; select.replaceChildren(reject, ...options());
        const rows = await load('domains?organization=' + organization);
        table(['Domain', 'Unknown recipients', 'Status', ''], rows.map(d => [d.name, node('span', d.catch_all || 'Reject', 'mono'), d.enabled ? 'Enabled' : 'Disabled', button('Edit', () => { const f = $('domain-form'); f.elements.name.value = d.name; f.elements.catch_all.value = d.catch_all; f.elements.enabled.checked = d.enabled; f.elements.name.focus(); })]));
      }
      if (tab === 'aliases') {
        $('alias-form').elements.inboxes.replaceChildren(...options()); const rows = await load('aliases?organization=' + organization);
        table(['Address', 'Destination inboxes', 'Status', ''], rows.map(a => [a.address, node('span', a.inboxes.join(', '), 'mono'), a.enabled ? 'Enabled' : 'Disabled', button('Edit', () => { const f = $('alias-form'); f.elements.address.value = a.address; f.elements.enabled.checked = a.enabled; Array.from(f.elements.inboxes.options).forEach(o => o.selected = a.inboxes.includes(o.value)); f.elements.address.focus(); })]));
      }
      if (tab === 'mailaccounts') {
        $('mail-account-form').elements.inbox.replaceChildren(...options()); const rows = await load('mail-accounts?organization=' + organization);
        table(['Username', 'Inbox', 'Status', ''], rows.map(a => [a.username, node('span', a.inbox, 'mono'), a.enabled ? 'Enabled' : 'Disabled', (a.enabled || canRead()) ? button(a.enabled ? 'Disable' : 'Enable', async () => { await load('mail-access', {organization, username: a.username, enabled: !a.enabled}); await show(); }) : '']));
      }
    }
    if (tab === 'storage') {
      const u = await load('storage-usage?organization=' + organization), f = $('storage-quota-form');
      f.elements.limit_bytes.value = u.limit_bytes || ''; f.elements.disable_limit.checked = u.limit_bytes === null;
      table(['Metric', 'Value'], [
        ['Retained originals', u.retained_originals], ['Retained original bytes', u.retained_bytes], ['Threshold bytes', u.limit_bytes ?? 'Disabled'],
        ['Current usage measured', new Date(u.measured_at).toLocaleString()], ['Alert from last sample', u.alert_active ? 'Threshold exceeded' : 'No active alert'],
        ['Last sample', u.sampled_at ? new Date(u.sampled_at).toLocaleString() : 'Not sampled'], ['Sample freshness', u.sample_stale ? 'Stale or awaiting evaluation' : 'Recent'],
        ['Policy version', u.policy_version], ['Sampled policy version', u.sampled_policy_version ?? 'Not sampled'], ['Sampled retained originals', u.sampled_originals ?? 'Not sampled'], ['Sampled retained bytes', u.sampled_bytes ?? 'Not sampled'], ['Last transition', u.last_transition_state ? u.last_transition_state + ' · ' + new Date(u.last_transition_at).toLocaleString() : 'None']
      ]);
    }
    if (tab === 'memberstorage') {
      if (!globalOperator()) { $('listing').replaceChildren(node('p', 'Global operator access is required.', 'empty')); return; }
      if (!storageMember) { $('listing').replaceChildren(node('p', 'Enter a configured member ID to inspect storage.', 'empty')); return; }
      const query = new URLSearchParams({member: storageMember, after: storageAfter, limit: '64'});
      const snapshot = await load('member-storage?' + query);
      memberStorage(snapshot);
      if (snapshot.next_file) { $('next').hidden = false; $('next').onclick = () => task(async () => { if (!active()) return; storageAfter = snapshot.next_file; await show(); }); }
    }
    if (tab === 'submission') {
      const rows = await load('submission-accounts?organization=' + organization);
      for (const id of ['submission-password-form', 'submission-policy-form']) {
        const f = $(id); f.elements.username.replaceChildren(...rows.map(a => { const option = node('option', a.username); option.value = a.username; return option; })); f.hidden = rows.length === 0;
      }
      const policy = $('submission-policy-form');
      const fillPolicy = () => { const a = rows.find(a => a.username === policy.elements.username.value); policy.elements.authorized_senders.value = (a?.authorized_senders || []).join('\n'); policy.elements.authorized_domains.value = (a?.authorized_domains || []).join('\n'); };
      policy.elements.username.onchange = fillPolicy; fillPolicy();
      table(['Username', 'Permitted senders', 'Status', ''], rows.map(a => [a.username, [...(a.authorized_senders || []), ...(a.authorized_domains || [])].join(', '), a.enabled ? 'Enabled' : 'Disabled', button(a.enabled ? 'Disable' : 'Enable', async () => { await load('submission-access', {organization, username: a.username, enabled: !a.enabled}); await show(); })]));
    }
    if (tab === 'senders') {
      const rows = await load('sender-grants?organization=' + organization);
      table(['Sender scope', 'Status', 'Evidence', 'Reason', ''], rows.map(g => [g.scope, g.active ? 'Authorized' : 'Inactive', g.evidence || 'Awaiting verification', g.reason || '', g.active ? button('Revoke', async () => { await load('sender-revoke', {organization, scope: g.scope, reason: 'Revoked through administration'}); if (senderChallenge?.organization === organization && senderChallenge.scope === g.scope) senderChallenge = null; await show(); }) : '']));
    }
    if (tab === 'messages') {
      const rows = await load('messages?organization=' + organization + '&after=' + after);
      table(['Receipt', 'Received', 'Size', 'State', ''], rows.map(m => {
        const a = node('a', 'Download'); a.href = '/api/content?organization=' + organization + '&receipt=' + m.id;
        return [node('span', m.id, 'mono'), new Date(m.received).toLocaleString(), (m.size / 1024).toFixed(1) + ' KB', m.state, canRead() && m.state === 'live' ? a : ''];
      }));
      if (rows.length === 100) { $('next').hidden = false; $('next').onclick = () => task(async () => { after = rows.at(-1).id; await show(); }); }
    }
    if (tab === 'audit') {
      const rows = await load('audit?before=' + before);
      table(['Time', 'Action', 'Actor / target', 'Outcome'], rows.map(a => [new Date(a.at).toLocaleString(), a.action, node('span', a.actor + ' / ' + a.target, 'mono'), a.outcome]));
      if (rows.length === 100) { $('next').hidden = false; $('next').onclick = () => task(async () => { before = rows.at(-1).id; await show(); }); }
    }
    if (tab === 'users') {
      users = await load('users');
      table(['Account', 'Access', 'Status', ''], users.map(u => {
        const scopes = node('div');
        for (const g of u.grants) {
          const line = node('div', (g.scope === '*' ? 'Global operator' : organizations.find(o => o.id === g.scope)?.name || g.scope) + (g.content ? ' · content' : '') + ' · ' + g.source);
          if (g.source === 'local') line.append(button('Remove', async () => { await load('grants', {user: u.id, grants: u.grants.filter(x => x.source === 'local' && x.scope !== g.scope)}); await show(); }));
          scopes.append(line);
        }
        return [u.name + ' · ' + u.method, scopes, u.enabled ? 'Enabled' : 'Disabled', button(u.enabled ? 'Disable & revoke sessions' : 'Enable', async () => { await load('access', {user: u.id, enabled: !u.enabled}); await show(); })];
      }));
      const select = $('grant-form').elements.user; select.replaceChildren(...users.map(u => { const option = node('option', u.name); option.value = u.id; return option; }));
    }
    loaded = true;
  } catch (e) {
    if (active() && tab === 'memberstorage') $('listing').replaceChildren(node('p', 'Member storage diagnostics are unavailable. Retry or inspect another configured member.', 'empty'));
    throw e;
  } finally { if (active() && (loaded || tab === 'memberstorage')) loading(false); }
}
function form(id, action) { $(id).addEventListener('submit', (event) => { event.preventDefault(); if (id === 'login-form' && signingOut) return; const f = event.currentTarget; const submit = f.querySelector('button'); task(async () => { submit.disabled = true; try { await action(f); } finally { submit.disabled = false; } }); }); }
form('login-form', async f => { const who = await api('login', {username: f.elements.username.value, password: f.elements.password.value}); f.elements.password.value = ''; await signedIn(who); });
form('organization-form', async f => { const result = await api('organizations', {name: f.elements.name.value}); f.reset(); organization = result.id; await signedIn(await api('session')); });
form('inbox-form', async () => { await api('inboxes', {organization}); await show(); notice('Inbox created.'); });
form('domain-form', async f => { await api('domains', {organization, name: f.elements.name.value, catch_all: f.elements.catch_all.value, enabled: f.elements.enabled.checked}); f.reset(); await show(); notice('Incoming domain saved.'); });
form('alias-form', async f => { await api('aliases', {organization, address: f.elements.address.value, inboxes: Array.from(f.elements.inboxes.selectedOptions).map(o => o.value), enabled: f.elements.enabled.checked}); f.reset(); await show(); notice('Incoming address saved.'); });
form('mail-account-form', async f => { await api('mail-accounts', {organization, username: f.elements.username.value, password: f.elements.password.value, inbox: f.elements.inbox.value, enabled: true}); f.reset(); await show(); notice('IMAP credential saved.'); });
form('storage-quota-form', async f => {
  const org = organization, generation = viewGeneration, who = identity;
  const limit = f.elements.disable_limit.checked ? null : f.elements.limit_bytes.value.trim();
  if (limit !== null && !/^[0-9]+$/.test(limit)) throw new Error('Enter a positive whole number of bytes, or disable the threshold.');
  try { await api('storage-quota', {organization: org, limit_bytes: limit}); }
  catch (e) { if (identity !== who || organization !== org || generation !== viewGeneration) throw staleView; throw e; }
  if (identity !== who || organization !== org || generation !== viewGeneration) return;
  await show(); notice('Advisory storage threshold saved.');
});
form('member-storage-form', async f => {
  if (!identity || !globalOperator() || tab !== 'memberstorage') return;
  const member = f.elements.member.value.trim().toLowerCase();
  if (!/^[0-9a-f]{32}$/.test(member)) throw new Error('Enter a configured member ID of 32 hexadecimal characters.');
  storageMember = member; storageAfter = ''; f.elements.member.value = member; await show();
});
function senderEntries(value) { return value.split(/[,\r\n]/).map(v => v.trim()).filter(Boolean); }
async function submissionEdit(f, create) {
  const org = organization, generation = viewGeneration, who = identity;
  const input = {organization: org, username: f.elements.username.value, authorized_senders: senderEntries(f.elements.authorized_senders.value), authorized_domains: senderEntries(f.elements.authorized_domains.value)};
  if (create) { input.password = f.elements.password.value; input.enabled = f.elements.enabled.checked; f.elements.password.value = ''; }
  try { await api(create ? 'submission-accounts' : 'submission-policy', input); }
  catch (e) { if (identity !== who || organization !== org || generation !== viewGeneration) throw staleView; throw e; }
  if (identity !== who || organization !== org || generation !== viewGeneration) return;
  f.reset(); await show(); notice(create ? 'SMTP credential created.' : 'Permitted senders saved.');
}
form('submission-create-form', f => submissionEdit(f, true));
form('submission-policy-form', f => submissionEdit(f, false));
form('submission-password-form', async f => {
  const org = organization, generation = viewGeneration, who = identity;
  const input = {organization: org, username: f.elements.username.value, password: f.elements.password.value};
  f.elements.password.value = '';
  try { await api('submission-password', input); }
  catch (e) { if (identity !== who || organization !== org || generation !== viewGeneration) throw staleView; throw e; }
  if (identity !== who || organization !== org || generation !== viewGeneration) return;
  await show(); notice('SMTP password rotated.');
});
form('sender-challenge-form', async f => {
  const org = organization, generation = viewGeneration, who = identity;
  const result = await api('sender-challenge', {organization: org, scope: f.elements.scope.value});
  if (identity !== who || organization !== org || generation !== viewGeneration) return;
  senderChallenge = result; await show(); notice('Publish the TXT record, then check DNS below.');
});
form('sender-verify-form', async f => {
  const org = organization, generation = viewGeneration, who = identity;
  await api('sender-verify', {organization: org, challenge: f.elements.challenge.value});
  if (identity !== who || organization !== org || generation !== viewGeneration) return;
  senderChallenge = null; f.reset(); await show(); notice('DNS verified. Sender authorized.');
});
form('sender-override-form', async f => {
  const org = organization, generation = viewGeneration, who = identity;
  await api('sender-override', {organization: org, scope: f.elements.scope.value, reason: f.elements.reason.value});
  if (identity !== who || organization !== org || generation !== viewGeneration) return;
  senderChallenge = null; f.reset(); await show(); notice('Override recorded. Sender authorized.');
});
form('user-form', async f => { await api('users', {username: f.elements.username.value, password: f.elements.password.value, enabled: true}); f.reset(); await show(); notice('Account saved. Assign access below.'); });
form('grant-form', async f => {
  const user = users.find(u => u.id === f.elements.user.value), scope = f.elements.scope.value === '*' ? '*' : organization;
  if (!scope) throw new Error('Select an organization first.');
  const grants = user.grants.filter(g => g.source === 'local' && g.scope !== scope).map(g => ({scope: g.scope, content: g.content}));
  grants.push({scope, content: f.elements.content.checked}); await api('grants', {user: user.id, grants}); await show(); notice('Access assignment saved.');
});
$('logout').onclick = () => task(async () => {
  if (signingOut) return;
  signingOut = true;
  const oidcHidden = $('oidc').hidden, request = api('logout', {});
  signedOut();
  const controls = $('login-form').querySelectorAll('input, button');
  controls.forEach(el => el.disabled = true); $('oidc').hidden = true;
  try { await request; }
  finally { signingOut = false; controls.forEach(el => el.disabled = false); $('oidc').hidden = oidcHidden; }
});
$('refresh').onclick = () => task(async () => { after = ''; before = 0; storageAfter = ''; await show(); });
document.querySelectorAll('[data-tab]').forEach(b => b.onclick = () => task(async () => { tab = b.dataset.tab; after = ''; before = 0; storageAfter = ''; await show(); }));
(async () => { try { const cfg = await api('config'); $('oidc').hidden = !cfg.oidc; try { await signedIn(await api('session')); } catch (error) { if (!FRYAdmin.unauthenticated(error)) throw error; } } catch (e) { if (e !== staleView) notice(e.message, true); } })();
