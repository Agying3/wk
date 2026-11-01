import test from 'node:test';
import assert from 'node:assert/strict';
import { ACCOUNT_BATCH_SIZE, runLocalAccountBatch, usesLocalAccountBatch } from './accountManagement.js';

const receipt = uids => ({ results: uids.map(uid => ({ uid, status: 'success', detail: 'saved' })) });

test('1000 local updates use ten requests, with fixed deduplicated targets', async () => {
  const targets = Array.from({ length: 1000 }, (_, i) => `u${i}`);
  targets.push('u0');
  const requests = [], progress = [];
  const rows = await runLocalAccountBatch(targets, async uids => {
    requests.push([...uids]); targets.push('unexpected');
    return receipt(uids);
  }, { onResult: (_, count) => progress.push(count) });
  assert.equal(requests.length, 10);
  assert.ok(requests.every(uids => uids.length === ACCOUNT_BATCH_SIZE));
  assert.equal(rows.length, 1000); assert.equal(progress.at(-1), 1000);
  assert.equal(rows.some(row => row.uid === 'unexpected'), false);
});
test('batch cancellation waits for the submitted chunk and never submits the next one', async () => {
  const targets = Array.from({ length: 125 }, (_, i) => `u${i}`);
  let stop = false, calls = 0;
  const rows = await runLocalAccountBatch(targets, async uids => { calls++; stop = true; return receipt(uids); }, { cancelled: () => stop });
  assert.equal(calls, 1);
  assert.equal(rows.filter(r => r.status === 'success').length, 100);
  assert.equal(rows.filter(r => r.status === 'cancelled').length, 25);
});
test('partial account failures are preserved while later chunks still run', async () => {
  const rows = await runLocalAccountBatch(Array.from({ length: 101 }, (_, i) => `u${i}`), async uids => {
    const body = receipt(uids); body.results[0].status = 'failed'; return body;
  });
  assert.equal(rows.length, 101);
  assert.equal(rows.filter(r => r.status === 'failed').length, 2);
});
test('storage failure or uncertain receipt stops later chunks without replay', async () => {
  for (const response of ['network', 'malformed', 'storage']) {
    let calls = 0;
    const rows = await runLocalAccountBatch(Array.from({ length: 101 }, (_, i) => `u${i}`), async uids => {
      calls++;
      if (response === 'network') throw new Error('connection lost');
      if (response === 'malformed') return { results: [] };
      return { stop: true, results: uids.map(uid => ({ uid, status: 'failed', detail: 'storage failed' })) };
    });
    assert.equal(calls, 1);
    assert.equal(rows.filter(r => r.status === 'failed').length, 100);
    assert.equal(rows.at(-1).status, 'cancelled');
  }
});
test('local dispatch excludes provider operations and deletion', () => {
  for (const action of ['enable', 'disable', 'clear-cooldown', 'groups']) assert.equal(usesLocalAccountBatch(action), true);
  for (const action of ['checkin', 'keepalive', 'delete', undefined]) assert.equal(usesLocalAccountBatch(action), false);
});
