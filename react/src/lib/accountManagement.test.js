import test from 'node:test';
import assert from 'node:assert/strict';
import { ACCOUNT_BATCH_CONCURRENCY, accountState, filterAccounts, groupChange, runAccountBatch, accountCSV } from './accountManagement.js';

const deferred = () => {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
};

const filters = { search: '', region: 'all', status: 'all', group: 'all', expiry: 'all' };
test('account status includes auth quarantine and model cooldown, ignores Go zero dates', () => {
  assert.equal(accountState({ until: '0001-01-01T00:00:00Z' }), 'healthy');
  assert.equal(accountState({ auth_until: '2030-01-01T00:00:00Z' }, 1), 'cooling');
  assert.equal(accountState({ model_cooldowns: { model: {} } }), 'model_cooling');
  assert.equal(accountState({ disabled: true, cooling: true }), 'disabled');
});
test('filters search full UID and combine group, region and expiry across the full pool', () => {
  const accounts = Array.from({ length: 1000 }, (_, i) => ({ uid: `uid-${i}`, region: 'cn', token_expires_at: 5 }));
  assert.deepEqual(filterAccounts(accounts, { 'uid-999': ['vip'] }, { ...filters, search: 'UID-999', group: 'vip', expiry: 'expired' }, 10000).map(a => a.uid), ['uid-999']);
  assert.equal(filterAccounts(accounts, {}, { ...filters, region: 'global' }).length, 0);
});
test('group append/remove preserve unrelated memberships and empty removal returns default', () => {
  assert.deepEqual(groupChange(['a', 'b'], ['b', 'c'], 'append'), ['a', 'b', 'c']);
  assert.deepEqual(groupChange(['a', 'b'], ['b'], 'remove'), ['a']);
  assert.deepEqual(groupChange(['a'], ['a'], 'remove'), ['default']);
});
test('batch freezes and deduplicates targets; reports HTTP 200 application failures and exceptions', async () => {
  const input = ['a', 'b', 'a', 'c']; const calls = [];
  const result = await runAccountBatch(input, async uid => {
    calls.push(uid); input.push('unexpected');
    if (uid === 'b') return { ok: false, detail: 'provider rejected' };
    if (uid === 'c') throw new Error('HTTP 409 busy');
    return { ok: true };
  }, { concurrency: 1 });
  assert.deepEqual(calls, ['a', 'b', 'c']);
  assert.deepEqual(result.map(r => r.status), ['success', 'failed', 'failed']);
});
test('cancel waits for current request but never sends subsequent writes', async () => {
  let stop = false; const calls = [];
  const result = await runAccountBatch(['a', 'b', 'c'], async uid => { calls.push(uid); stop = true; }, { cancelled: () => stop, concurrency: 1 });
  assert.deepEqual(calls, ['a']); assert.deepEqual(result.map(r => r.status), ['success', 'cancelled', 'cancelled']);
});
test('authentication loss, rate limiting or network failure stops the remaining queue', async () => {
  for (const status of [401, 403, 429, undefined]) {
    const calls = [];
    const result = await runAccountBatch(['a', 'b'], async uid => { calls.push(uid); const e = new Error('unavailable'); e.status = status; throw e; }, { concurrency: 1 });
    assert.deepEqual(calls, ['a']); assert.deepEqual(result.map(r => r.status), ['failed', 'cancelled']);
  }
});
test('bounded workers refill free slots without waiting for a slow account; results retain target order', async () => {
  const gates = new Map(['a', 'b', 'c', 'd', 'e'].map(uid => [uid, deferred()]));
  const calls = [], reports = [];
  const thirdStarted = deferred(), fourthStarted = deferred(), fifthStarted = deferred();
  let active = 0, peak = 0;
  const batch = runAccountBatch(['a', 'b', 'c', 'd', 'e', 'a'], async uid => {
    calls.push(uid); peak = Math.max(peak, ++active);
    if (uid === 'c') thirdStarted.resolve();
    if (uid === 'd') fourthStarted.resolve();
    if (uid === 'e') fifthStarted.resolve();
    try { return await gates.get(uid).promise; } finally { active--; }
  }, { concurrency: 2, onResult: (row, count) => reports.push([row.uid, count]) });
  assert.deepEqual(calls, ['a', 'b']);
  gates.get('b').resolve({ ok: false, detail: 'rejected' }); await thirdStarted.promise;
  gates.get('c').reject(Object.assign(new Error('busy'), { status: 409 })); await fourthStarted.promise;
  gates.get('d').resolve(); await fifthStarted.promise;
  gates.get('e').resolve(); gates.get('a').resolve();
  const result = await batch;
  assert.equal(peak, 2);
  assert.deepEqual(result.map(row => row.uid), ['a', 'b', 'c', 'd', 'e']);
  assert.deepEqual(result.map(row => row.status), ['success', 'failed', 'failed', 'success', 'success']);
  assert.deepEqual(reports.slice(0, 3), [['b', 1], ['c', 2], ['d', 3]]);
  assert.deepEqual(reports.map(([, count]) => count), [1, 2, 3, 4, 5]);
});
test('default and oversized worker counts never exceed four accounts', async () => {
  for (const concurrency of [undefined, 100]) {
    const gate = deferred(), calls = [];
    const batch = runAccountBatch(['a', 'b', 'c', 'd', 'e'], async uid => { calls.push(uid); await gate.promise; }, { concurrency });
    assert.equal(calls.length, ACCOUNT_BATCH_CONCURRENCY);
    gate.resolve();
    assert.equal((await batch).length, 5);
  }
});
test('parallel cancellation drains sent requests and marks only unsent accounts as cancelled', async () => {
  let stop = false; const gate = deferred(), calls = [];
  const batch = runAccountBatch(['a', 'b', 'c', 'd', 'e', 'f'], async uid => { calls.push(uid); await gate.promise; }, { cancelled: () => stop });
  assert.deepEqual(calls, ['a', 'b', 'c', 'd']);
  stop = true; gate.resolve();
  const rows = await batch;
  assert.deepEqual(calls, ['a', 'b', 'c', 'd']);
  assert.deepEqual(rows.map(r => r.status), ['success', 'success', 'success', 'success', 'cancelled', 'cancelled']);
});
test('a fatal parallel request stops dispatch and prevents group writes after pending reads', async () => {
  for (const status of [401, 403, 429, undefined]) {
    const gates = [deferred(), deferred()], stopped = deferred(); const calls = [], writes = [];
    const batch = runAccountBatch(['a', 'b', 'c', 'd'], async (uid, { cancelled }) => {
      calls.push(uid);
      if (uid === 'a') { await gates[0].promise; throw Object.assign(new Error('unavailable'), { status }); }
      await gates[1].promise;
      if (cancelled()) return { cancelled: true, detail: '未修改分组' };
      writes.push(uid);
    }, { concurrency: 2, onResult: row => { if (row.uid === 'a') stopped.resolve(); } });
    gates[0].resolve(); await stopped.promise;
    gates[1].resolve();
    const rows = await batch;
    assert.deepEqual(calls, ['a', 'b']); assert.deepEqual(writes, []);
    assert.deepEqual(rows.map(row => row.status), ['failed', 'cancelled', 'cancelled', 'cancelled']);
  }
});
test('empty or initially cancelled queues send no requests', async () => {
  const action = () => assert.fail('must not dispatch');
  assert.deepEqual(await runAccountBatch([], action), []);
  assert.deepEqual((await runAccountBatch(['a'], action, { cancelled: () => true })).map(r => r.status), ['cancelled']);
});
test('inventory CSV quotes fields and neutralizes spreadsheet formulas without exporting credentials', () => {
  const csv = accountCSV([{ uid: 'a', nickname: '=EVIL()', reason: 'one,"two"', accessToken: 'never-export' }], {});
  assert.ok(csv.includes("'=EVIL()")); assert.ok(csv.includes('one,""two""'));
  assert.ok(!csv.includes('never-export'));
});
