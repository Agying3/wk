import test from 'node:test';
import assert from 'node:assert/strict';
import {
  PLAYGROUND_ERROR_LIMIT, PLAYGROUND_HISTORY_LIMIT, PLAYGROUND_JSON_LIMIT, PLAYGROUND_SSE_FRAME_LIMIT, PLAYGROUND_TEXT_LIMIT,
  buildPlaygroundRequest, createPlaygroundSSEDecoder, createPlaygroundStream, defaultPlaygroundForm,
  parsePlaygroundError, parsePlaygroundJSON, playgroundAccounts, playgroundUsage, readPlaygroundBody,
  readPlaygroundResponse, reconcilePlaygroundSelection, rememberPlaygroundRun,
} from './playground.js';

const form = changes => ({ ...defaultPlaygroundForm, model: 'test-model', ...changes });
const frame = (data, event = '') => ({ event, data: typeof data === 'string' ? data : JSON.stringify(data) });
const sse = body => `data: ${JSON.stringify(body)}\n\n`;
function response(chunks, options = {}) {
  const encoder = new TextEncoder();
  let cancelled = false;
  const body = new ReadableStream({
    start(controller) { chunks.forEach(chunk => controller.enqueue(typeof chunk === 'string' ? encoder.encode(chunk) : chunk)); controller.close(); },
    cancel() { cancelled = true; },
  });
  return { response: new Response(body, { headers: { 'content-type': 'text/event-stream', 'x-request-id': 'req-test' }, ...options }), cancelled: () => cancelled };
}

test('request builder uses administrator endpoints, protocol-specific standard fields and optional defaults', () => {
  const responses = buildPlaygroundRequest(form({ system: 'Be brief', group: 'vip', account_uid: 'u1', max_tokens: 123, temperature: 0, reasoning_effort: 'high', timeout: 5 }));
  assert.equal(responses.path, '/admin/playground/responses');
  assert.equal(responses.timeoutMs, 5000);
  assert.deepEqual(responses.envelope, { request: { model: 'test-model', stream: true, input: defaultPlaygroundForm.prompt, instructions: 'Be brief', max_output_tokens: 123, reasoning: { effort: 'high' }, temperature: 0 }, group: 'vip', account_uid: 'u1' });
  const chat = buildPlaygroundRequest(form({ protocol: 'chat', system: 'System', max_tokens: 12, reasoning_effort: 'low' }));
  assert.equal(chat.path, '/admin/playground/chat/completions');
  assert.deepEqual(chat.envelope.request.messages, [{ role: 'system', content: 'System' }, { role: 'user', content: defaultPlaygroundForm.prompt }]);
  assert.equal(chat.envelope.request.max_tokens, 12);
  assert.equal(chat.envelope.request.reasoning_effort, 'low');
  assert.deepEqual(chat.envelope.request.stream_options, { include_usage: true });
  const plain = buildPlaygroundRequest(form({ protocol: 'chat', stream: false, Authorization: 'secret', headers: { Authorization: 'secret' } }));
  assert.equal(plain.timeoutMs, null);
  for (const key of ['temperature', 'max_tokens', 'reasoning_effort', 'stream_options', 'Authorization', 'headers']) assert.equal(key in plain.envelope.request, false);
  assert.equal(JSON.stringify(plain).includes('secret'), false);
});

test('request builder rejects malformed limits, blank input and unknown protocol/effort before sending', () => {
  for (const patch of [{ protocol: 'other' }, { model: '' }, { prompt: ' ' }, { max_tokens: 0 }, { max_tokens: 1.5 }, { max_tokens: 1000001 }, { temperature: -1 }, { temperature: NaN }, { timeout: 0 }, { timeout: 3601 }, { reasoning_effort: 'unknown' }]) {
    assert.throws(() => buildPlaygroundRequest(form(patch)));
  }
});

test('group/account reconciliation removes vanished, incompatible, disabled and unavailable selections without enabling or switching accounts', () => {
  const options = { groups: [{ name: 'vip' }, { name: 'other' }], accounts: [
    { uid: 'a', available: true, groups: ['vip'] }, { uid: 'b', available: true, groups: ['other'] },
    { uid: 'disabled', disabled: true, available: true, groups: ['vip'] }, { uid: 'cool', available: false, groups: ['vip'], unavailable_reason: 'cooldown' },
  ] };
  assert.deepEqual(playgroundAccounts(options, 'other').map(account => account.uid), ['b']);
  assert.deepEqual(reconcilePlaygroundSelection(options, { group: 'vip', account_uid: 'a' }), { group: 'vip', account_uid: 'a' });
  for (const account_uid of ['b', 'disabled', 'cool', 'missing']) assert.deepEqual(reconcilePlaygroundSelection(options, { group: 'vip', account_uid }), { group: 'vip', account_uid: '' });
  assert.deepEqual(reconcilePlaygroundSelection(options, { group: 'removed', account_uid: 'missing' }), { group: '', account_uid: '' });
  assert.deepEqual(reconcilePlaygroundSelection(options, { group: 'removed', account_uid: 'a' }), { group: '', account_uid: '' });
  assert.equal(options.accounts[2].disabled, true);
});

test('HTTP errors preserve structured causes, type, code and request ID; HTML/text are diagnostic text, never inferred model errors', () => {
  const headers = new Headers({ 'content-type': 'text/html', 'x-request-id': 'header-id' });
  const html = parsePlaygroundError('<html><script>never execute</script>Denied</html>', 403, headers);
  assert.equal(html.status, 403); assert.equal(html.request_id, 'header-id');
  assert.match(html.source_hint, /反向代理/); assert.match(html.body_preview, /<script>/);
  const json = parsePlaygroundError(JSON.stringify({ error: { message: 'no_healthy_account: TLS handshake: context deadline exceeded', type: 'upstream_error', code: 'no_healthy_account', request_id: 'body-id' } }), 503, headers);
  assert.match(json.message, /TLS handshake/); assert.equal(json.type, 'upstream_error'); assert.equal(json.code, 'no_healthy_account'); assert.equal(json.request_id, 'body-id'); assert.equal(json.source_hint, '');
  const redacted = parsePlaygroundError('Authorization: Bearer private-secret {"refreshToken":"fake-secret"}', 403);
  assert.equal(JSON.stringify(redacted).includes('private-secret'), false); assert.equal(JSON.stringify(redacted).includes('fake-secret'), false);
});

test('SSE decoder handles arbitrary LF/CRLF transport splits, comments, event names and multiline data', () => {
  const frames = [], parser = createPlaygroundSSEDecoder(value => frames.push(value));
  for (const chunk of [': ping\r', '\nevent: result\r\ndata: {"a":\r', '\ndata: 1}\r\n\r', '\ndata: [DONE]\n\n']) parser.push(chunk);
  parser.push('', true);
  assert.deepEqual(frames, [{ event: 'result', data: '{"a":\n1}' }, { event: '', data: '[DONE]' }]);
});

test('Responses stream retains output and usage but requires a valid completed event', () => {
  const stream = createPlaygroundStream('responses');
  stream.frame(frame({ type: 'response.output_text.delta', delta: 'Hello ' }));
  stream.frame(frame({ type: 'response.output_text.delta', delta: 'world' }));
  assert.equal(stream.snapshot().text, 'Hello world');
  stream.frame(frame({ type: 'response.completed', response: { id: 'resp-1', status: 'completed', output: [{ content: [{ type: 'output_text', text: 'Hello world' }] }], usage: { input_tokens: 0, output_tokens: 2 } } }));
  assert.equal(stream.finish().state, 'completed'); assert.equal(stream.finish().responseID, 'resp-1'); assert.equal(stream.finish().usage.input_tokens, 0);
  for (const suffix of [null, frame('[DONE]')]) {
    const broken = createPlaygroundStream('responses'); broken.frame(frame({ type: 'response.output_text.delta', delta: 'partial' }));
    if (suffix) broken.frame(suffix);
    assert.equal(broken.finish().state, 'incomplete'); assert.equal(broken.finish().text, 'partial'); assert.equal(broken.finish().error.code, 'unexpected_eof');
  }
});

test('Responses reads native reasoning_text deltas and final reasoning without duplicating summaries', () => {
  const stream = createPlaygroundStream('responses');
  stream.frame(frame({ type: 'response.reasoning_text.delta', delta: '先思考' }));
  assert.equal(stream.snapshot().reasoning, '先思考');
  stream.frame(frame({ type: 'response.output_text.delta', delta: '答案' }));
  stream.frame(frame({ type: 'response.completed', response: { status: 'completed', output: [
    { type: 'reasoning', content: [{ type: 'reasoning_text', text: '完整思考' }], summary: [{ type: 'summary_text', text: '完整思考' }] },
    { type: 'message', content: [{ type: 'output_text', text: '答案' }] },
  ] } }));
  assert.equal(stream.finish().reasoning, '完整思考');
  assert.equal(stream.finish().text, '答案');
  assert.equal(stream.finish().state, 'completed');
});

test('Responses nonstream reads summary-only reasoning and caps reasoning output', () => {
  const parsed = parsePlaygroundJSON({ status: 'completed', output: [{ type: 'reasoning', summary: [{ type: 'summary_text', text: '摘要' }] }] }, 'responses');
  assert.equal(parsed.reasoning, '摘要');
  const large = parsePlaygroundJSON({ status: 'completed', output: [{ type: 'reasoning', content: [{ type: 'reasoning_text', text: 'x'.repeat(PLAYGROUND_TEXT_LIMIT + 1) }] }] }, 'responses');
  assert.equal(large.reasoning.length, PLAYGROUND_TEXT_LIMIT);
  assert.equal(large.truncated, true);
});

test('SSE response.failed, response.incomplete, error events and malformed JSON never become successful', () => {
  const failed = createPlaygroundStream('responses');
  failed.frame(frame({ type: 'response.output_text.delta', delta: 'partial' }));
  failed.frame(frame({ type: 'response.failed', response: { error: { message: 'upstream_timeout', code: 'timeout' } } }));
  assert.equal(failed.finish().state, 'failed'); assert.equal(failed.finish().error.code, 'timeout'); assert.equal(failed.finish().text, 'partial');
  const incomplete = createPlaygroundStream('responses'); incomplete.frame(frame({ type: 'response.incomplete', response: { output_text: 'limited', incomplete_details: { reason: 'max_output_tokens' } } }));
  assert.equal(incomplete.finish().state, 'incomplete'); assert.match(incomplete.finish().error.message, /max_output_tokens/);
  const error = createPlaygroundStream('chat'); error.frame(frame({ error: { message: 'rate limited', type: 'rate_limit', code: 429 } }));
  assert.equal(error.finish().state, 'failed'); assert.equal(error.finish().error.code, '429');
  const invalid = createPlaygroundStream('responses'); invalid.frame(frame('not JSON'));
  assert.equal(invalid.finish().state, 'failed'); assert.equal(invalid.finish().error.code, 'invalid_json');
});

test('Chat stream requires finish_reason plus DONE and retains the trailing usage frame', () => {
  const stream = createPlaygroundStream('chat');
  stream.frame(frame({ id: 'chat-1', choices: [{ index: 0, delta: { content: 'Hello', reasoning_content: 'Thinking' }, finish_reason: null }] }));
  stream.frame(frame({ choices: [{ index: 0, delta: {}, finish_reason: 'stop' }] }));
  stream.frame(frame({ choices: [], usage: { prompt_tokens: 4, completion_tokens: 2, prompt_tokens_details: { cached_tokens: 0 } } }));
  stream.frame(frame('[DONE]'));
  assert.equal(stream.finish().state, 'completed'); assert.equal(stream.finish().responseID, 'chat-1'); assert.equal(stream.finish().usage.prompt_tokens, 4);
  const noDone = createPlaygroundStream('chat'); noDone.frame(frame({ choices: [{ delta: { content: 'partial' }, finish_reason: 'stop' }] }));
  assert.equal(noDone.finish().state, 'incomplete');
  const noFinish = createPlaygroundStream('chat'); noFinish.frame(frame('[DONE]')); assert.equal(noFinish.finish().state, 'incomplete');
  const length = createPlaygroundStream('chat'); length.frame(frame({ choices: [{ delta: { content: 'cut off' }, finish_reason: 'length' }] })); length.frame(frame('[DONE]'));
  assert.equal(length.finish().state, 'incomplete');
});

test('nonstream completion and usage distinguish incomplete/missing metadata, zero and unknown', () => {
  assert.equal(parsePlaygroundJSON({ status: 'incomplete', incomplete_details: { reason: 'limit' } }, 'responses').state, 'incomplete');
  assert.equal(parsePlaygroundJSON({ output_text: 'partial' }, 'responses').state, 'failed');
  assert.equal(parsePlaygroundJSON({ choices: [{ message: { content: 'ok' }, finish_reason: 'stop' }] }, 'chat').state, 'completed');
  assert.equal(parsePlaygroundJSON({ choices: [{ message: { content: 'partial' } }] }, 'chat').state, 'failed');
  assert.deepEqual(playgroundUsage(null), { input: null, output: null, cached: null, reasoning: null });
  assert.deepEqual(playgroundUsage({ input_tokens: 0, output_tokens: 2, input_tokens_details: { cached_tokens: 0 }, output_tokens_details: { reasoning_tokens: 1 } }), { input: 0, output: 2, cached: 0, reasoning: 1 });
});

test('transport reads fragmented UTF8 SSE, retains correlation ID, and detects premature EOF', async () => {
  const bytes = new TextEncoder().encode(sse({ type: 'response.output_text.delta', delta: '你好' }) + sse({ type: 'response.completed', response: { status: 'completed', output_text: '你好' } }));
  const chunks = Array.from(bytes, byte => Uint8Array.of(byte));
  const updates = [];
  const result = await readPlaygroundResponse(response(chunks).response, 'responses', true, value => updates.push(value));
  assert.equal(result.state, 'completed'); assert.equal(result.text, '你好'); assert.equal(result.requestID, 'req-test'); assert.ok(updates.length >= 2);
  const partial = await readPlaygroundResponse(response([sse({ type: 'response.output_text.delta', delta: 'partial' })]).response, 'responses', true);
  assert.equal(partial.state, 'incomplete'); assert.equal(partial.text, 'partial');
});

test('transport separates HTTP failures and content-type mismatches from response text', async () => {
  const forbidden = await readPlaygroundResponse(response(['<html>management route blocked</html>'], { status: 403, headers: { 'content-type': 'text/html' } }).response, 'responses', true);
  assert.equal(forbidden.state, 'failed'); assert.equal(forbidden.text, ''); assert.equal(forbidden.error.status, 403); assert.match(forbidden.error.source_hint, /WAF/);
  const unavailable = await readPlaygroundResponse(response([JSON.stringify({ error: { message: 'TLS handshake timeout', code: 'no_healthy_account' } })], { status: 503, headers: { 'content-type': 'application/json' } }).response, 'chat', false);
  assert.match(unavailable.error.message, /TLS handshake/);
  const mismatch = await readPlaygroundResponse(response(['{"error":{"message":"specific root cause"}}'], { headers: { 'content-type': 'application/json' } }).response, 'responses', true);
  assert.equal(mismatch.state, 'failed'); assert.match(mismatch.error.message, /specific root cause/); assert.match(mismatch.error.message, /text\/event-stream/);
});

test('response readers cap JSON/error bytes and cancel an oversized reader without consuming the tail', async () => {
  let reads = 0, cancelled = false;
  const bounded = { body: { getReader: () => ({ async read() { reads++; return { done: false, value: new Uint8Array(16) }; }, async cancel() { cancelled = true; }, releaseLock() {} }) } };
  const body = await readPlaygroundBody(bounded, 20);
  assert.equal(body.truncated, true); assert.equal(reads, 2); assert.equal(cancelled, true); assert.equal(body.text.length, 20);
  const large = await readPlaygroundResponse(response(['x'.repeat(PLAYGROUND_JSON_LIMIT + 1)], { headers: { 'content-type': 'application/json' } }).response, 'responses', false);
  assert.equal(large.state, 'failed'); assert.equal(large.error.code, 'body_limit');
  const error = await readPlaygroundResponse(response(['x'.repeat(PLAYGROUND_ERROR_LIMIT + 1)], { status: 403 }).response, 'responses', false);
  assert.equal(error.error.body_truncated, true); assert.match(error.error.message, /截断/); assert.ok(error.error.body_preview.length <= 12000);
});

test('SSE limit counts multiple data lines and fails safely with partial output preserved', async () => {
  const parser = createPlaygroundSSEDecoder(() => {});
  parser.push(`data: ${'x'.repeat(PLAYGROUND_SSE_FRAME_LIMIT / 2)}\n`);
  assert.throws(() => parser.push(`data: ${'y'.repeat(PLAYGROUND_SSE_FRAME_LIMIT / 2)}\n`), error => error.code === 'sse_frame_limit');
  const oneLine = createPlaygroundSSEDecoder(() => {});
  assert.throws(() => oneLine.push('x'.repeat(PLAYGROUND_SSE_FRAME_LIMIT + 1)));
  const huge = await readPlaygroundResponse(response([sse({ type: 'response.output_text.delta', delta: 'partial' }), `data: ${'x'.repeat(PLAYGROUND_SSE_FRAME_LIMIT + 1)}\n\n`]).response, 'responses', true);
  assert.equal(huge.state, 'failed'); assert.equal(huge.error.code, 'sse_frame_limit'); assert.equal(huge.text, 'partial');
});

test('output display cap preserves protocol completion and reports truncation', () => {
  const stream = createPlaygroundStream('chat');
  stream.frame(frame({ choices: [{ delta: { content: 'x'.repeat(PLAYGROUND_TEXT_LIMIT + 1) }, finish_reason: 'stop' }] })); stream.frame(frame('[DONE]'));
  assert.equal(stream.finish().state, 'completed'); assert.equal(stream.finish().text.length, PLAYGROUND_TEXT_LIMIT); assert.equal(stream.finish().truncated, true);
});

test('bounded history whitelists request fields, copies the form, and never retains auth headers or arbitrary response fields', () => {
  let history = [];
  const input = form({ Authorization: 'secret', headers: { Authorization: 'secret' } });
  for (let i = 0; i < 14; i++) history = rememberPlaygroundRun(history, { id: String(i), at: i, form: input, state: 'failed', status: 403, duration: 5, rawHeaders: 'secret' });
  assert.equal(history.length, PLAYGROUND_HISTORY_LIMIT); assert.equal(history[0].id, '13');
  input.prompt = 'changed'; assert.equal(history[0].form.prompt, defaultPlaygroundForm.prompt);
  assert.equal(JSON.stringify(history).includes('secret'), false);
});
