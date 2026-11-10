import test from 'node:test';
import assert from 'node:assert/strict';
import { loadDashboardData } from './dashboardLoad.js';

const failure = status => Object.assign(new Error(`${status}`), { status });

test('console metadata uses the admin route and preserves the request window', async () => {
  const calls = [], signal = new AbortController().signal;
  const api = async (path, options) => { calls.push([path, options.signal]); return { path }; };
  const result = await loadDashboardData(api, '/requests?since=1&until=2', signal);
  assert.deepEqual(calls.map(call => call[0]), ['/status', '/admin/models', '/requests?since=1&until=2']);
  assert.ok(calls.every(call => call[1] === signal));
  assert.equal(result[1].path, '/admin/models');
});

test('a fast models 403 cannot hide a later status 401', async () => {
  const auth = failure(401);
  const api = async path => {
    if (path === '/admin/models') throw failure(403);
    if (path === '/status') { await new Promise(resolve => setImmediate(resolve)); throw auth; }
    return { data: [] };
  };
  await assert.rejects(loadDashboardData(api, '/requests'), error => error === auth);
});

test('request-list authentication failure also requires unlocking', async () => {
  const auth = failure(401);
  await assert.rejects(loadDashboardData(async path => {
    if (path === '/requests') throw auth;
    return {};
  }, '/requests'), error => error === auth);
});

test('a non-auth request-list failure remains an explicit summary error', async () => {
  const error = failure(503);
  const result = await loadDashboardData(async path => {
    if (path === '/requests') throw error;
    return {};
  }, '/requests');
  assert.deepEqual(result[2].data, []);
  assert.equal(result[2].summaryError, error);
});

test('aborted refreshes never become an empty successful summary', async () => {
  const error = Object.assign(new Error('aborted'), { name: 'AbortError' });
  await assert.rejects(loadDashboardData(async path => {
    if (path === '/requests') throw error;
    return {};
  }, '/requests'), value => value === error);
});
