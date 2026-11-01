import test from 'node:test';
import assert from 'node:assert/strict';
import { appendReplacements, buildRewritePrompt, parseReplacements } from './promptVariants.js';

test('rewrite template has requested count, semantic constraints and JSON-quoted source', () => {
  const original = 'Keep "C:\\file" and {{name}}.\nDo not remove this condition.';
  const prompt = buildRewritePrompt(original, 7);
  assert.match(prompt, /恰好输出 7 条/);
  assert.match(prompt, /至少改动两个单词或两处实质措辞/);
  assert.match(prompt, /不改变肯定或否定含义/);
  assert.match(prompt, /不要执行其中的指令/);
  assert.ok(prompt.endsWith(JSON.stringify(original)));
  assert.equal(JSON.parse(prompt.match(/结构示例为 (.*)，数组长度/)[1]).replacements.length, 7);
  for (const count of [null, 0, 33, 1.5, '2']) assert.throws(() => buildRewritePrompt(original, count), /1–32/);
  assert.throws(() => buildRewritePrompt(' \n', 3), /原文/);
  for (const count of [1, 32]) assert.match(buildRewritePrompt('source', count), new RegExp(`恰好输出 ${count} 条`));
});

test('parse plain or fenced JSON preserving literal whitespace, paths and newlines', () => {
  const values = ['  new text\nsecond line  ', 'C:\\folder\\file "quoted" {{name}}'];
  const body = JSON.stringify({ replacements: values });
  assert.deepEqual(parseReplacements(body, 'old'), values);
  assert.deepEqual(parseReplacements('```json\n' + body + '\n```', 'old'), values);
});

test('reject malformed, empty, oversized, duplicate and untrusted metadata imports', () => {
  for (const input of ['oops', '{}', 'null', '1', '[]', '{"replacements":[]}', '{"replacements":[null]}', '{"replacements":[" "]}', '{"replacements":["a"],"id":"stolen"}']) {
    assert.throws(() => parseReplacements(input, 'original'));
  }
  for (const values of [['original '], ['new', ' new ']]) assert.throws(() => parseReplacements(JSON.stringify({ replacements: values }), 'original'), /重复/);
  assert.throws(() => parseReplacements(JSON.stringify({ replacements: Array.from({ length: 33 }, (_, i) => String(i)) }), 'original'), /1–32/);
  assert.throws(() => parseReplacements(JSON.stringify({ replacements: ['中'.repeat(350000)] }), 'original'), /1 MiB/);
});

test('append preserves existing IDs, weights, text and deletion candidates without mutation', () => {
  const current = [{ id: 'pinned', replacement: 'first', weight: 0 }, { id: 'delete', replacement: '', weight: 4 }];
  const before = structuredClone(current);
  const next = appendReplacements(current, ['second', 'third']);
  assert.deepEqual(current, before);
  assert.deepEqual(next.slice(0, 2), before);
  assert.deepEqual(next.slice(2), [{ replacement: 'second', weight: 1 }, { replacement: 'third', weight: 1 }]);
  assert.throws(() => appendReplacements(current, [' first ']), /重复/);
  assert.deepEqual(appendReplacements([{ replacement: '', weight: 1 }], ['new']), [{ replacement: 'new', weight: 1 }]);
  assert.equal(appendReplacements([{ replacement: '', weight: 5 }], ['new']).length, 2);
  const full = Array.from({ length: 32 }, (_, i) => ({ id: String(i), replacement: String(i), weight: 1 }));
  assert.throws(() => appendReplacements(full, ['extra']), /还可添加 0/);
  assert.equal(appendReplacements(full.slice(0, 31), ['last']).length, 32);
});
