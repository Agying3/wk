import test from 'node:test';
import assert from 'node:assert/strict';
import { ruleTestView } from './promptRuleTest.js';

test('rule test requires a passing control and complete before/after evidence', () => {
  const trace = [{ step: 'control', outcome: 'pass' }, { step: 'original', outcome: 'blocked' }, { step: 'replacement', outcome: 'pass' }];
  assert.equal(ruleTestView({ trace }).type, 'success');
  assert.equal(ruleTestView({ trace, stale: true }).type, 'warning');
  assert.equal(ruleTestView({ trace, cancelled: true }).type, 'warning');
  assert.equal(ruleTestView({ trace: [] }), null);
  assert.equal(ruleTestView({ trace: [{ step: 'control', outcome: 'blocked' }] }).type, 'warning');
  assert.equal(ruleTestView({ trace: trace.slice(0, 2) }).type, 'warning');
  assert.equal(ruleTestView({ trace: trace.map(row => ({ ...row, outcome: 'pass' })) }).type, 'info');
  assert.match(ruleTestView({ trace: [...trace.slice(0, 2), { step: 'replacement', outcome: 'blocked' }] }).title, /仍触发/);
});
