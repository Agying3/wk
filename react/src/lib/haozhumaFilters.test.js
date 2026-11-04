import test from 'node:test';
import assert from 'node:assert/strict';
import { haozhumaCardTypes, haozhumaOperators, haozhumaProvinces, savedOperatorValues, monitorOperatorValue } from './haozhumaFilters.js';

test('official carrier values and old UI selections stay consistent', () => {
  assert.deepEqual(haozhumaOperators.map(x => x.value), ['1', '5', '9', '14', '16']);
  assert.deepEqual(savedOperatorValues('1,2,3,5'), ['1', '5', '9']);
  assert.deepEqual(savedOperatorValues('14,16'), ['14', '16']);
});

test('monitor can leave inherited policy and can explicitly restore it', () => {
  assert.equal(monitorOperatorValue(['inherit', '5']), '5');
  assert.equal(monitorOperatorValue(['5', 'inherit']), null);
  assert.equal(monitorOperatorValue([]), '');
  assert.equal(monitorOperatorValue(['5', '9']), '5,9');
});

test('province selectors use the supplied official province codes', () => {
  assert.equal(haozhumaProvinces.length, 31);
  assert.equal(haozhumaProvinces.find(p => p.label === '广东').value, '44');
  assert.equal(haozhumaProvinces.find(p => p.label === '北京').value, '11');
  assert.equal(haozhumaProvinces.find(p => p.label === '新疆').value, '65');
  assert.equal(new Set(haozhumaProvinces.map(p => p.value)).size, 31);
});

test('real and virtual card mappings follow the official ascription contract', () => {
  assert.equal(haozhumaCardTypes.find(x => x.label === '实卡').value, '2');
  assert.equal(haozhumaCardTypes.find(x => x.label === '虚拟号').value, '1');
});
