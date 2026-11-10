import test from 'node:test';
import assert from 'node:assert/strict';
import { accountDetailSections, accountDetailTime, accountDetailTimeMillis } from './accountDetails.js';

const section = (sections, key) => sections.find(value => value.key === key);
const value = (sections, group, key) => section(sections, group).rows.find(row => row.key === key).value;

test('detail timestamps keep Unix seconds separate from RFC3339 deadlines and reject unknown/Go zero times', () => {
  assert.equal(accountDetailTimeMillis(1900000000, 'seconds'), 1900000000000);
  assert.equal(accountDetailTimeMillis('2030-01-02T03:04:05+08:00'), Date.parse('2030-01-01T19:04:05Z'));
  for (const time of [null, undefined, '', 'invalid', '0001-01-01T00:00:00Z']) assert.equal(accountDetailTimeMillis(time), null);
  for (const time of [0, -1, null, '1900000000', NaN]) assert.equal(accountDetailTimeMillis(time, 'seconds'), null);
  assert.equal(accountDetailTime(0, 'seconds'), '未知');
});

test('stored Mac profile exposes the documented hardware and masked identifier fields', () => {
  const details = { device: { profile_source: 'stored', identity_present: true, profile: {
    version: 1, platform: 'macos', arch: 'arm64', os_version: '24.6.0', cpu_model: 'Apple M2', cpu_cores: 8, memory_gib: 16, timezone: 'Asia/Shanghai',
  }, identifiers: { machine_id: 'ABCD…WXYZ', cli_session_id: '1234…7890' } } };
  const sections = accountDetailSections(details);
  for (const [key, expected] of Object.entries({ profile_source: '已保存档案', platform: 'macOS', arch: 'arm64', os_version: '24.6.0', cpu_model: 'Apple M2', cpu_cores: '8', memory_gib: '16', timezone: 'Asia/Shanghai', version: '1', identity_present: '是' })) {
    assert.equal(value(sections, 'device', key), expected);
  }
  assert.equal(value(sections, 'identifiers', 'machine_id'), 'ABCD…WXYZ');
  assert.equal(value(sections, 'identifiers', 'session_id'), '—');
});

test('legacy Windows profile is explicitly unsaved and derived, without suggesting Mac creation', () => {
  const sections = accountDetailSections({ device: { profile_source: 'legacy_derived', identity_present: false, profile: { platform: 'windows', arch: 'x64' } } });
  assert.equal(value(sections, 'device', 'platform'), 'Windows');
  assert.equal(value(sections, 'device', 'profile_source'), '未保存 / 兼容推导');
  assert.equal(value(sections, 'device', 'identity_present'), '否');
  assert.match(section(sections, 'device').note, /不会生成新设备/);
  assert.equal(section(sections, 'device').rows.some(row => /macOS|新建 Mac/.test(String(row.value))), false);
});

test('credits require a collected snapshot while explicit zero counters/capacities are preserved', () => {
  const status = { uid: 'u1', disabled: false, credits: 0, estimated_spent: 0, effective_credits: 0, cycle_capacity_used: 0, cycle_capacity_size: 0, cycle_capacity_remain: 0, capacity_used: 0, capacity_size: 0, capacity_remain: 0, in_flight: 0, breaker_fails: 0, risk_strikes: 0 };
  const unknown = accountDetailSections(null, status);
  assert.equal(value(unknown, 'status', 'credits'), '未知');
  assert.equal(value(unknown, 'status', 'in_flight'), '0');
  const known = accountDetailSections({ status: { ...status, credit_updated_at: 1900000000 } });
  for (const key of ['credits', 'estimated_spent', 'effective_credits', 'cycle_capacity_remain', 'capacity_remain', 'risk_strikes', 'breaker_fails']) assert.equal(value(known, 'status', key), '0');
  assert.equal(value(known, 'status', 'cycle_capacity'), '0 / 0');
  assert.equal(value(known, 'status', 'capacity'), '0 / 0');
  assert.equal(value(accountDetailSections({ status: { credit_updated_at: 1900000000 } }), 'status', 'credits'), '未知');
});

test('field allowlist excludes tokens, unknown properties, raw profile identifiers and credential payloads', () => {
  const secret = 'never-render-this-credential';
  const details = {
    uid: 'u1', domain: 'example.test', enterprise_id: 'enterprise', account_type: 'personal', oneid_account_id: 'oneid', idp: 'idp', department_full_name: 'department', token_type: 'Bearer',
    accessToken: secret, unknown: secret, auth: { accessToken: secret },
    credential: { has_access_token: true, has_refresh_token: false, accessToken: secret, refreshToken: secret, unknown: secret },
    device: { profile: { platform: 'windows', machine_id: secret, session_id: secret, accessToken: secret }, identifiers: { machine_id: 'abcd…wxyz', unknown: secret } },
    status: { unknown: secret, accessToken: secret },
  };
  const sections = accountDetailSections(details);
  assert.equal(JSON.stringify(sections).includes(secret), false);
  for (const key of ['domain', 'enterprise_id', 'account_type', 'oneid_account_id', 'idp', 'department_full_name', 'token_type']) assert.equal(value(sections, 'basic', key), details[key]);
  assert.equal(value(sections, 'credential', 'has_access_token'), '是');
  assert.equal(value(sections, 'credential', 'has_refresh_token'), '否');
});

test('import fallback marker stays distinct from current disabled status and missing device data stays unknown', () => {
  const sections = accountDetailSections({ credential: { import_disabled: true }, status: { uid: 'u1', disabled: false } });
  assert.equal(value(sections, 'credential', 'import_disabled'), '是');
  assert.equal(value(sections, 'status', 'disabled'), '否');
  assert.equal(value(sections, 'status', 'state'), '可用');
  assert.match(section(sections, 'credential').note, /不等同于当前禁用/);
  assert.equal(value(sections, 'device', 'platform'), '—');
  assert.equal(value(sections, 'device', 'cpu_cores'), '未知');
});

test('failed or pending detail loads retain the supplied overview without inventing identity metadata', () => {
  const sections = accountDetailSections(null, { uid: 'u2', nickname: 'Overview', region: 'global', disabled: true, reason: 'manual disabled', token_expires_at: 1900000000 }, ['vip']);
  assert.equal(value(sections, 'basic', 'uid'), 'u2');
  assert.equal(value(sections, 'basic', 'groups'), 'vip');
  assert.equal(value(sections, 'status', 'state'), '已禁用');
  assert.equal(value(sections, 'status', 'reason'), '管理员手动禁用');
  assert.equal(value(sections, 'credential', 'expires_at'), accountDetailTime(1900000000, 'seconds'));
  assert.equal(value(sections, 'basic', 'enterprise_id'), '—');
  assert.equal(value(sections, 'identifiers', 'machine_id'), '—');
});
