export const PLAYGROUND_HISTORY_LIMIT = 10;
export const PLAYGROUND_TEXT_LIMIT = 200000;
export const PLAYGROUND_JSON_LIMIT = 2 * 1024 * 1024;
export const PLAYGROUND_ERROR_LIMIT = 64 * 1024;
export const PLAYGROUND_SSE_FRAME_LIMIT = 512 * 1024;
export const defaultPlaygroundForm = {
  protocol: 'responses', model: '', prompt: '你好，请简短介绍一下你自己。', system: '', stream: true,
  group: '', account_uid: '', max_tokens: null, temperature: null, reasoning_effort: '', timeout: null,
};

const string = value => typeof value === 'string' ? value : typeof value === 'number' ? String(value) : '';
const optionalNumber = value => value === null || value === undefined || value === '' ? null : Number(value);

export function buildPlaygroundRequest(form) {
  if (!['responses', 'chat'].includes(form.protocol)) throw new Error('请选择请求协议');
  if (!form.model?.trim()) throw new Error('请选择模型');
  if (!form.prompt?.trim()) throw new Error('请输入测试内容');
  const max = optionalNumber(form.max_tokens), temperature = optionalNumber(form.temperature), timeout = optionalNumber(form.timeout);
  if (max !== null && (!Number.isInteger(max) || max < 1 || max > 1000000)) throw new Error('最大输出 tokens 需为 1–1000000 的整数');
  if (temperature !== null && (!Number.isFinite(temperature) || temperature < 0 || temperature > 2)) throw new Error('Temperature 需在 0–2 之间');
  if (timeout !== null && (!Number.isFinite(timeout) || timeout < 1 || timeout > 3600)) throw new Error('浏览器超时需在 1–3600 秒之间');
  if (form.reasoning_effort && !['none', 'minimal', 'low', 'medium', 'high', 'xhigh'].includes(form.reasoning_effort)) throw new Error('推理档位无效');
  const request = { model: form.model.trim(), stream: !!form.stream };
  if (form.protocol === 'responses') {
    request.input = form.prompt;
    if (form.system?.trim()) request.instructions = form.system;
    if (max !== null) request.max_output_tokens = max;
    if (form.reasoning_effort) request.reasoning = { effort: form.reasoning_effort };
  } else {
    request.messages = [...(form.system?.trim() ? [{ role: 'system', content: form.system }] : []), { role: 'user', content: form.prompt }];
    if (max !== null) request.max_tokens = max;
    if (form.reasoning_effort) request.reasoning_effort = form.reasoning_effort;
    if (form.stream) request.stream_options = { include_usage: true };
  }
  if (temperature !== null) request.temperature = temperature;
  return {
    path: `/admin/playground/${form.protocol === 'responses' ? 'responses' : 'chat/completions'}`,
    envelope: { request, group: form.group || '', account_uid: form.account_uid || '' },
    timeoutMs: timeout === null ? null : timeout * 1000,
  };
}

export function playgroundAccounts(options, group = '') {
  return (options?.accounts || []).filter(account => !group || account.groups?.includes(group));
}

export function reconcilePlaygroundSelection(options, selection) {
  const group = !selection.group || options.groups?.some(item => item.name === selection.group) ? selection.group || '' : '';
  if (selection.group && !group) return { group: '', account_uid: '' };
  const account = playgroundAccounts(options, group).find(item => item.uid === selection.account_uid && item.available === true && !item.disabled);
  return { group, account_uid: account?.uid || '' };
}

export function redactPlaygroundSecrets(value) {
  return string(value).replace(/\bBearer\s+[A-Za-z0-9._~+/=-]+/gi, 'Bearer [REDACTED]')
    .replace(/("(?:access_?token|refresh_?token|api[_-]?key|authorization)"\s*:\s*")[^"]*(")/gi, '$1[REDACTED]$2');
}

export function parsePlaygroundError(raw, status, headers) {
  let body;
  try { body = typeof raw === 'string' ? JSON.parse(raw) : raw; } catch { body = null; }
  const error = body?.error ?? body;
  const isJSON = body !== null && typeof body === 'object';
  const html = !isJSON && (/<(?:!doctype|html|head|body)\b/i.test(string(raw)) || headers?.get?.('content-type')?.includes('text/html'));
  const message = string(error?.message || (typeof error === 'string' ? error : '')) || (isJSON ? `请求失败（HTTP ${status}）` : html ? '服务返回 HTML 错误页' : '服务返回非 JSON 错误内容');
  return {
    status, message: redactPlaygroundSecrets(message).slice(0, 12000), type: string(error?.type ?? body?.type), code: string(error?.code ?? body?.code),
    request_id: string(error?.request_id || body?.request_id || body?.requestId || headers?.get?.('x-request-id') || headers?.get?.('request-id')),
    body_preview: !isJSON ? redactPlaygroundSecrets(raw).slice(0, 12000) : '',
    source_hint: !isJSON ? `${html ? 'HTML 页面' : '非 JSON 文本'}可能来自反向代理、WAF 或网关；请结合 HTTP 状态和请求 ID 核对，不能据此断言是模型错误。` : '',
  };
}

export function playgroundUsage(usage) {
  const finite = value => typeof value === 'number' && Number.isFinite(value) ? value : null;
  return {
    input: finite(usage?.input_tokens ?? usage?.prompt_tokens), output: finite(usage?.output_tokens ?? usage?.completion_tokens),
    cached: finite(usage?.input_tokens_details?.cached_tokens ?? usage?.prompt_tokens_details?.cached_tokens ?? usage?.cache_read_tokens),
    reasoning: finite(usage?.output_tokens_details?.reasoning_tokens ?? usage?.completion_tokens_details?.reasoning_tokens),
  };
}

function responseText(body) {
  if (typeof body?.output_text === 'string') return body.output_text;
  return (Array.isArray(body?.output) ? body.output : []).flatMap(item => Array.isArray(item?.content) ? item.content : []).filter(item => item?.type === 'output_text').map(item => string(item.text)).join('');
}

const incompleteError = (message, code = 'incomplete') => ({ message, type: 'incomplete_response', code, request_id: '' });

export function parsePlaygroundJSON(body, protocol) {
  const base = { text: '', reasoning: '', usage: body?.usage || null, responseID: string(body?.id), state: 'failed', error: null };
  if (body?.error) return { ...base, error: parsePlaygroundError(body, 200) };
  if (protocol === 'responses') {
    base.text = responseText(body);
    if (body?.status === 'completed') base.state = 'completed';
    else if (body?.status === 'incomplete') { base.state = 'incomplete'; base.error = incompleteError(`响应未完成：${string(body?.incomplete_details?.reason) || '原因未提供'}`); }
    else base.error = incompleteError(`响应未完成，status=${string(body?.status) || '缺失'}`, 'missing_completion');
  } else {
    const choice = body?.choices?.[0];
    base.text = typeof choice?.message?.content === 'string' ? choice.message.content : '';
    base.reasoning = string(choice?.message?.reasoning_content);
    if (['stop', 'tool_calls', 'function_call'].includes(choice?.finish_reason)) base.state = 'completed';
    else if (choice?.finish_reason) { base.state = 'incomplete'; base.error = incompleteError(`响应未完成：${choice.finish_reason}`); }
    else base.error = incompleteError('缺少有效的 choices/finish_reason，无法确认响应完成', 'missing_completion');
  }
  base.truncated = base.text.length > PLAYGROUND_TEXT_LIMIT || base.reasoning.length > PLAYGROUND_TEXT_LIMIT;
  base.text = base.text.slice(0, PLAYGROUND_TEXT_LIMIT); base.reasoning = base.reasoning.slice(0, PLAYGROUND_TEXT_LIMIT);
  return base;
}

// SSE supports CRLF, LF, comments, multiple data lines and arbitrary transport chunks.
export function createPlaygroundSSEDecoder(onFrame) {
  let buffer = '', data = [], event = '', frameSize = 0;
  const limit = () => { const error = new Error('SSE 单个事件超过 512 Ki 字符，已停止读取'); error.code = 'sse_frame_limit'; throw error; };
  const line = value => {
    if (value === '') { if (data.length) onFrame({ event, data: data.join('\n') }); data = []; event = ''; frameSize = 0; return; }
    frameSize += value.length + 1;
    if (frameSize > PLAYGROUND_SSE_FRAME_LIMIT) limit();
    if (value.startsWith(':')) return;
    const colon = value.indexOf(':');
    const field = colon < 0 ? value : value.slice(0, colon);
    let content = colon < 0 ? '' : value.slice(colon + 1);
    if (content.startsWith(' ')) content = content.slice(1);
    if (field === 'data') data.push(content);
    if (field === 'event') event = content;
  };
  return {
    push(chunk, final = false) {
      buffer += chunk;
      let match;
      while ((match = /[\r\n]/.exec(buffer))) {
        const at = match.index;
        if (!final && buffer[at] === '\r' && at === buffer.length - 1) break;
        const width = buffer[at] === '\r' && buffer[at + 1] === '\n' ? 2 : 1;
        line(buffer.slice(0, at)); buffer = buffer.slice(at + width);
      }
      if (final) { if (buffer) line(buffer); buffer = ''; line(''); }
      if (buffer.length + frameSize > PLAYGROUND_SSE_FRAME_LIMIT) limit();
    },
  };
}

export function createPlaygroundStream(protocol) {
  let state = { state: 'running', text: '', reasoning: '', usage: null, responseID: '', error: null, truncated: false };
  let done = false, finishReason = '';
  const append = (key, value) => {
    if (typeof value !== 'string') return;
    const next = state[key] + value;
    state.truncated ||= next.length > PLAYGROUND_TEXT_LIMIT;
    state[key] = next.slice(0, PLAYGROUND_TEXT_LIMIT);
  };
  const fail = error => { state.state = 'failed'; state.error = error; done = true; };
  return {
    frame(frame) {
      if (done) return;
      if (frame.data === '[DONE]') {
        if (protocol === 'chat' && finishReason) {
          done = true;
          state.state = ['stop', 'tool_calls', 'function_call'].includes(finishReason) ? 'completed' : 'incomplete';
          if (state.state === 'incomplete') state.error = incompleteError(`响应未完成：${finishReason}`);
        }
        return;
      }
      let body;
      try { body = JSON.parse(frame.data); }
      catch { fail({ message: 'SSE 返回了无法解析的 JSON 数据', type: 'invalid_sse', code: 'invalid_json', request_id: '', body_preview: redactPlaygroundSecrets(frame.data).slice(0, 12000) }); return; }
      const type = body?.type || frame.event;
      state.responseID = string(body?.response?.id || body?.id) || state.responseID;
      if (body?.usage) state.usage = body.usage;
      if (body?.error || type === 'error' || type === 'response.failed') {
        fail(parsePlaygroundError(body?.response?.error ? { error: body.response.error } : body, 200)); return;
      }
      if (protocol === 'responses') {
        if (type === 'response.output_text.delta') append('text', body.delta);
        if (type === 'response.reasoning_summary_text.delta') append('reasoning', body.delta);
        if (type === 'response.completed' || type === 'response.incomplete') {
          const final = parsePlaygroundJSON({ ...body.response, status: type === 'response.incomplete' ? 'incomplete' : body.response?.status }, protocol);
          state = { ...state, ...final, text: final.text || state.text, reasoning: state.reasoning, usage: final.usage || state.usage, truncated: state.truncated || final.truncated }; done = true;
        }
      } else {
        const choice = body?.choices?.find(item => item.index === 0 || item.index === undefined);
        append('text', choice?.delta?.content); append('reasoning', choice?.delta?.reasoning_content);
        if (choice?.finish_reason) finishReason = choice.finish_reason;
      }
    },
    snapshot() { return { ...state }; },
    terminal() { return done; },
    finish() {
      if (!done) { state.state = 'incomplete'; state.error = incompleteError('连接已结束，但未收到协议完成标记；已接收正文保留，不能确认请求成功。', 'unexpected_eof'); }
      return { ...state };
    },
  };
}

export async function readPlaygroundBody(response, limit) {
  const reader = response.body?.getReader();
  if (!reader) return { text: '', truncated: false };
  const decoder = new TextDecoder();
  let size = 0, text = '', truncated = false;
  try {
    while (true) {
      const chunk = await reader.read();
      if (chunk.done) { text += decoder.decode(); break; }
      const remaining = limit - size;
      if (chunk.value.byteLength > remaining) {
        text += decoder.decode(chunk.value.subarray(0, remaining)); truncated = true; break;
      }
      size += chunk.value.byteLength;
      text += decoder.decode(chunk.value, { stream: true });
    }
    return { text, truncated };
  } finally {
    try { await reader.cancel(); } catch { /* Already closed. */ }
    reader.releaseLock();
  }
}

export async function readPlaygroundResponse(response, protocol, streaming, onUpdate = () => {}) {
  const requestID = response.headers.get('x-request-id') || response.headers.get('request-id') || '';
  if (!response.ok) {
    const body = await readPlaygroundBody(response, PLAYGROUND_ERROR_LIMIT);
    const error = parsePlaygroundError(body.text, response.status, response.headers);
    if (body.truncated) { error.message += '（错误正文超过 64 KiB，已截断）'; error.body_truncated = true; }
    return { state: 'failed', text: '', usage: null, requestID, error };
  }
  if (!streaming) {
    const limited = await readPlaygroundBody(response, PLAYGROUND_JSON_LIMIT);
    const raw = limited.text;
    if (limited.truncated) return { state: 'failed', text: '', requestID, error: { message: 'JSON 响应超过 2 MiB，已停止读取，无法确认响应完整性。', type: 'response_too_large', code: 'body_limit', request_id: requestID } };
    let body;
    try { body = JSON.parse(raw); }
    catch { return { state: 'failed', text: '', requestID, error: parsePlaygroundError(raw, response.status, response.headers) }; }
    return { ...parsePlaygroundJSON(body, protocol), requestID };
  }
  if (!response.headers.get('content-type')?.toLowerCase().includes('text/event-stream')) {
    const body = await readPlaygroundBody(response, PLAYGROUND_ERROR_LIMIT);
    const error = parsePlaygroundError(body.text, response.status, response.headers);
    return { state: 'failed', text: '', requestID, error: { ...error, message: `${error.message}；流式请求未返回 text/event-stream，请核对代理及服务返回内容。${body.truncated ? '正文超过 64 KiB，已截断。' : ''}` } };
  }
  const reader = response.body?.getReader();
  if (!reader) throw new Error('浏览器未提供流式读取能力');
  const decoder = new TextDecoder();
  const stream = createPlaygroundStream(protocol);
  const parser = createPlaygroundSSEDecoder(frame => { stream.frame(frame); onUpdate({ ...stream.snapshot(), requestID }); });
  try {
    while (!stream.terminal()) {
      const chunk = await reader.read();
      parser.push(decoder.decode(chunk.value || new Uint8Array(), { stream: !chunk.done }), chunk.done);
      if (chunk.done) break;
    }
    return { ...stream.finish(), requestID };
  } catch (error) {
    if (error.code === 'sse_frame_limit') return { ...stream.snapshot(), state: 'failed', requestID, error: { message: error.message, type: 'invalid_sse', code: error.code, request_id: requestID } };
    throw error;
  } finally {
    try { await reader.cancel(); } catch { /* A broken/aborted connection is already closed. */ }
    reader.releaseLock();
  }
}

export function rememberPlaygroundRun(history, entry) {
  // Only explicit form fields are retained. Authentication headers never enter history.
  const form = Object.fromEntries(Object.keys(defaultPlaygroundForm).map(key => [key, entry.form[key] ?? defaultPlaygroundForm[key]]));
  return [{ id: entry.id, at: entry.at, form, state: entry.state, status: entry.status, duration: entry.duration }, ...history].slice(0, PLAYGROUND_HISTORY_LIMIT);
}
