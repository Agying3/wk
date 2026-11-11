import React, { useEffect, useRef, useState } from 'react';
import { Alert, App, Button, Card, Collapse, Descriptions, Empty, Form, Input, InputNumber, Select, Space, Switch, Table, Tag, Typography } from 'antd';
import { CopyOutlined, ReloadOutlined, SendOutlined, StopOutlined } from '@ant-design/icons';
import { buildPlaygroundRequest, defaultPlaygroundForm, playgroundAccounts, playgroundUsage, readPlaygroundResponse, reconcilePlaygroundSelection, redactPlaygroundSecrets, rememberPlaygroundRun } from '../lib/playground';
import './playground.css';

const { Text } = Typography;
const stateLabels = { running: '请求中', completed: '已完成', incomplete: '未完成', failed: '失败', cancelled: '已取消', timeout: '浏览器超时' };
const stateColors = { running: 'blue', completed: 'green', incomplete: 'orange', failed: 'red', cancelled: 'default', timeout: 'orange' };
const accountReasons = { disabled: '已停用', cooling_or_unhealthy: '冷却或暂不可用', busy: '并发已满', queued: '已有排队请求' };
const token = value => value === null || value === undefined ? '未知' : value.toLocaleString();
const seconds = value => value === null || value === undefined ? '—' : `${(value / 1000).toFixed(2)}s`;

export default function Playground({ api, headers = {}, models = [], onUnauthorized }) {
  const { message } = App.useApp();
  const [form, setForm] = useState({ ...defaultPlaygroundForm });
  const [options, setOptions] = useState({ groups: [], accounts: [] });
  const [optionsRevision, setOptionsRevision] = useState(0);
  const [optionsLoading, setOptionsLoading] = useState(true);
  const [optionsReady, setOptionsReady] = useState(false);
  const [optionsError, setOptionsError] = useState('');
  const [selectionNotice, setSelectionNotice] = useState('');
  const [busy, setBusy] = useState(false);
  const [run, setRun] = useState(null);
  const [history, setHistory] = useState([]);
  const [validationError, setValidationError] = useState('');
  const alive = useRef(true), active = useRef(null), unauthorized = useRef(onUnauthorized);
  const formRef = useRef(form); formRef.current = form;
  unauthorized.current = onUnauthorized;
  useEffect(() => { alive.current = true; return () => { alive.current = false; active.current?.controller.abort(); }; }, []);
  useEffect(() => {
    setForm(current => current.model || !models.length ? current : { ...current, model: models[0].id });
  }, [models]);
  useEffect(() => {
    let disposed = false;
    const controller = new AbortController();
    setOptionsLoading(true); setOptionsReady(false); setOptionsError('');
    api('/admin/playground/options', { signal: controller.signal, cache: 'no-store' }).then(body => {
      if (disposed) return;
      const next = { groups: Array.isArray(body.groups) ? body.groups : [], accounts: Array.isArray(body.accounts) ? body.accounts : [] };
      setOptions(next); setOptionsReady(true);
      const previous = formRef.current, valid = reconcilePlaygroundSelection(next, previous);
      if (valid.group !== previous.group || valid.account_uid !== previous.account_uid) setSelectionNotice('路由选项已变化，失效的分组或账号选择已清除。请核对后再发送。');
      setForm(current => ({ ...current, ...reconcilePlaygroundSelection(next, current) }));
    }).catch(error => {
      if (disposed) return;
      if (error.status === 401) unauthorized.current?.();
      setOptionsError(`读取分组和账号选项失败${error.status ? `（HTTP ${error.status}）` : ''}，请刷新选项后再发送。`);
      // Keep the user's scope on a failed refresh, but block sending until a fresh snapshot succeeds.
      // Clearing it here could silently widen a later retry to the entire pool.
    }).finally(() => { if (!disposed) setOptionsLoading(false); });
    return () => { disposed = true; controller.abort(); };
  }, [api, optionsRevision]);

  const change = patch => { setForm(current => ({ ...current, ...patch })); setValidationError(''); };
  const chooseGroup = group => {
    setForm(current => ({ ...current, ...reconcilePlaygroundSelection(options, { ...current, group: group || '' }) }));
    setSelectionNotice('');
  };
  let preview, previewError = '';
  try { preview = buildPlaygroundRequest(form); } catch (error) { previewError = error.message; }
  const copy = async value => {
    try { await navigator.clipboard.writeText(value); message.success('已复制'); }
    catch { message.warning('剪贴板不可用，请选择文本手动复制。'); }
  };
  const cancel = () => { if (active.current) { active.current.reason = 'cancelled'; active.current.controller.abort(); } };
  const send = async () => {
    if (active.current || !optionsReady) return;
    let request;
    try { request = buildPlaygroundRequest(form); } catch (error) { setValidationError(error.message); return; }
    const selection = reconcilePlaygroundSelection(options, form);
    if (selection.group !== form.group || selection.account_uid !== form.account_uid) {
      setForm(current => ({ ...current, ...selection })); setValidationError('选定分组或账号已不可用，已清除选择，请重新核对。'); return;
    }
    const snapshot = { ...form }, controller = new AbortController();
    const task = { controller, reason: '' }; active.current = task;
    const started = performance.now(), at = Date.now();
    let firstToken = null;
    let current = { state: 'running', text: '', reasoning: '', error: null, usage: null, duration: 0, ttft: null, httpStatus: null, requestID: '', responseID: '', form: snapshot };
    const update = partial => {
      if (firstToken === null && (partial.text || partial.reasoning)) firstToken = performance.now() - started;
      current = { ...current, ...partial, duration: performance.now() - started, ttft: firstToken, form: snapshot };
      if (alive.current && active.current === task) setRun(current);
    };
    setBusy(true); setValidationError(''); setRun(current);
    const timer = request.timeoutMs === null ? null : window.setTimeout(() => { task.reason = 'timeout'; controller.abort(); }, request.timeoutMs);
    try {
      const response = await fetch(request.path, {
        method: 'POST', credentials: 'same-origin', signal: controller.signal,
        headers: { ...headers, 'Content-Type': 'application/json' }, body: JSON.stringify(request.envelope),
      });
      update({ httpStatus: response.status });
      if (response.status === 401) unauthorized.current?.();
      update(await readPlaygroundResponse(response, snapshot.protocol, snapshot.stream, update));
    } catch (error) {
      if (controller.signal.aborted) {
        update({ state: task.reason === 'timeout' ? 'timeout' : 'cancelled', error: {
          message: task.reason === 'timeout' ? '达到本次设置的浏览器超时，已取消等待；已收到的正文保留。' : '已取消本次请求，已收到的正文保留。',
          type: task.reason === 'timeout' ? 'client_timeout' : 'client_cancelled', code: task.reason || 'cancelled', request_id: current.requestID,
        } });
      } else update({ state: 'failed', error: { message: `连接或读取失败：${redactPlaygroundSecrets(error.message || '网络错误').slice(0, 2000)}`, type: 'network_error', code: 'read_failed', request_id: current.requestID } });
    } finally {
      if (timer !== null) window.clearTimeout(timer);
      if (active.current === task) active.current = null;
      if (alive.current) {
        setBusy(false);
        setHistory(previous => rememberPlaygroundRun(previous, { id: `${at}`, at, form: snapshot, state: current.state, status: current.httpStatus, duration: current.duration }));
      }
    }
  };
  const loadHistory = entry => {
    const selection = reconcilePlaygroundSelection(options, entry.form);
    setForm({ ...entry.form, ...selection });
    setSelectionNotice('已载入历史参数；不会自动发送。请核对分组、账号与模型后手动点击发送。'); setValidationError('');
  };
  const usage = playgroundUsage(run?.usage);
  const diagnostic = run ? JSON.stringify({ protocol: run.form.protocol, model: run.form.model, group: run.form.group, account_uid: run.form.account_uid, state: run.state, http_status: run.httpStatus, request_id: run.error?.request_id || run.requestID, response_id: run.responseID, duration_ms: Math.round(run.duration), ttft_ms: run.ttft, usage: run.usage, error: run.error }, null, 2) : '';
  const modelOptions = models.map(model => ({ value: model.id, label: model.id }));
  if (form.model && !modelOptions.some(model => model.value === form.model)) modelOptions.unshift({ value: form.model, label: `${form.model}（目录未列出）` });
  return <div className="playground">
    <div className="playground-intro"><Text type="secondary">手动验证协议、路由与响应。发送会产生真实推理用量；选项为本机状态快照，实际可用性以发送时校验为准。</Text><Tag>管理员测试入口</Tag></div>
    <div className="playground-layout">
      <Card title="请求参数" className="playground-controls">
        <Form layout="vertical">
          <div className="playground-grid"><Form.Item label="协议"><Select aria-label="协议" value={form.protocol} disabled={busy} onChange={protocol => change({ protocol })} options={[{ value: 'responses', label: 'Responses API' }, { value: 'chat', label: 'Chat Completions' }]} /></Form.Item><Form.Item label="响应方式"><Space><Switch aria-label="流式响应" checked={form.stream} disabled={busy} onChange={stream => change({ stream })} /><Text>{form.stream ? '流式 SSE' : '非流式 JSON'}</Text></Space></Form.Item></div>
          <Form.Item label="模型"><Select aria-label="模型" showSearch optionFilterProp="label" value={form.model || undefined} disabled={busy} placeholder="搜索并选择模型" onChange={model => change({ model })} options={modelOptions} /></Form.Item>
          <div className="playground-route-heading"><Text strong>路由范围</Text><Button size="small" icon={<ReloadOutlined />} loading={optionsLoading} disabled={busy} onClick={() => setOptionsRevision(value => value + 1)}>刷新选项</Button></div>
          {optionsError && <Alert type="error" showIcon message={optionsError} />}
          {selectionNotice && <Alert type="info" showIcon closable onClose={() => setSelectionNotice('')} message={selectionNotice} />}
          <Form.Item label="分组"><Select aria-label="目标分组" showSearch optionFilterProp="label" value={form.group} loading={optionsLoading} disabled={busy || !optionsReady} onChange={chooseGroup} options={[{ value: '', label: '自动路由（全部授权范围）' }, ...options.groups.map(group => ({ value: group.name, label: `${group.name} · ${group.region || 'all'} · ${group.accounts ?? '—'} 个账号` }))]} /></Form.Item>
          <Form.Item label="账号" extra="指定账号仍受禁用、冷却与并发限制；此处不会启用账号或自动遍历账号。"><Select aria-label="目标账号" showSearch optionFilterProp="label" value={form.account_uid} disabled={busy || !optionsReady} onChange={account_uid => change({ account_uid })} options={[{ value: '', label: '由网关自动选择' }, ...playgroundAccounts(options, form.group).map(account => ({ value: account.uid, disabled: account.disabled || !account.available, label: `${account.nickname || '未命名'} · ${account.uid} · ${account.region}${account.disabled || !account.available ? ` · 不可用：${accountReasons[account.unavailable_reason] || (account.disabled ? '已停用' : '当前不可用')}` : ''}` }))]} /></Form.Item>
          <Form.Item label={form.protocol === 'responses' ? 'Instructions（可选）' : 'System message（可选）'}><Input.TextArea aria-label="系统指令" rows={3} value={form.system} disabled={busy} onChange={event => change({ system: event.target.value })} placeholder="角色、回答风格或其它系统指令" maxLength={200000} /></Form.Item>
          <Form.Item label="输入内容"><Input.TextArea aria-label="用户输入" rows={6} value={form.prompt} disabled={busy} onChange={event => change({ prompt: event.target.value })} placeholder="输入本次测试内容" maxLength={200000} showCount /></Form.Item>
          <Collapse size="small" items={[{ key: 'advanced', label: '生成参数与超时（留空沿用服务默认值）', children: <div className="playground-grid">
            <Form.Item label="最大输出 tokens"><InputNumber min={1} max={1000000} precision={0} value={form.max_tokens} disabled={busy} onChange={max_tokens => change({ max_tokens })} placeholder="默认" /></Form.Item>
            <Form.Item label="Temperature"><InputNumber min={0} max={2} step={0.1} value={form.temperature} disabled={busy} onChange={temperature => change({ temperature })} placeholder="默认" /></Form.Item>
            <Form.Item label="Reasoning effort"><Select value={form.reasoning_effort} disabled={busy} onChange={reasoning_effort => change({ reasoning_effort })} options={[{ value: '', label: '默认（不传）' }, ...['none', 'minimal', 'low', 'medium', 'high', 'xhigh'].map(value => ({ value, label: value }))]} /></Form.Item>
            <Form.Item label="浏览器等待超时（秒）"><InputNumber min={1} max={3600} value={form.timeout} disabled={busy} onChange={timeout => change({ timeout })} placeholder="不额外限制" /></Form.Item>
            <Text type="secondary" className="playground-full">可选参数是否支持由模型与服务决定；浏览器超时不修改服务端超时配置。</Text>
          </div> }]} />
          {(validationError || previewError) && <Alert type="warning" showIcon style={{ marginTop: 14 }} message={validationError || previewError} />}
          <Space wrap style={{ marginTop: 18 }}><Button aria-label="发送请求" type="primary" icon={<SendOutlined />} loading={busy} disabled={!!previewError || !optionsReady} onClick={send}>发送请求</Button>{busy && <Button aria-label="停止请求" icon={<StopOutlined />} onClick={cancel}>取消请求</Button>}</Space>
        </Form>
      </Card>
      <div className="playground-results">
        <Card title="响应" extra={run && <Space><Tag color={stateColors[run.state]}>{stateLabels[run.state]}</Tag><Button size="small" icon={<CopyOutlined />} onClick={() => copy(run.text || '')} disabled={!run.text}>复制正文</Button></Space>}>
          {run ? <>
            <Space wrap style={{ marginBottom: 12 }}><Tag>{run.form.protocol === 'responses' ? 'Responses' : 'Chat'}</Tag><Text>{run.form.model}</Text><Text type="secondary">{run.form.group || '自动分组'} / {run.form.account_uid || '自动账号'}</Text></Space>
            <div className="playground-metrics" aria-live="polite">{[['HTTP', run.httpStatus ?? '未收到响应'], ['耗时', seconds(run.duration)], ['首 token', seconds(run.ttft)], ['输入 tokens', token(usage.input)], ['输出 tokens', token(usage.output)], ['缓存命中 tokens', token(usage.cached)], ['推理 tokens', token(usage.reasoning)]].map(([label, value]) => <div key={label}><Text type="secondary">{label}</Text><strong>{value}</strong></div>)}</div>
            {(run.requestID || run.responseID) && <Descriptions size="small" column={1} items={[{ key: 'request', label: 'Request ID', children: <Text copyable={!!run.requestID}>{run.requestID || '—'}</Text> }, { key: 'response', label: 'Response ID', children: <Text copyable={!!run.responseID}>{run.responseID || '—'}</Text> }]} />}
            {run.error && <Alert className="playground-error" type={run.state === 'failed' ? 'error' : 'warning'} showIcon message={`${run.httpStatus ? `HTTP ${run.httpStatus} · ` : ''}${run.error.message}`} description={<Space direction="vertical" style={{ width: '100%' }}><Text>type: {run.error.type || '—'} · code: {run.error.code || '—'}</Text><Text copyable={!!(run.error.request_id || run.requestID)}>request_id: {run.error.request_id || run.requestID || '—'}</Text>{run.error.source_hint && <Text>{run.error.source_hint} 入口代理可能拒绝请求，请核对管理域名和接口路径。</Text>}{run.error.body_preview && <pre className="playground-raw">{run.error.body_preview}</pre>}<Button size="small" onClick={() => copy(diagnostic)}>复制诊断（不含认证头）</Button></Space>} />}
            <pre className="playground-output" aria-live="polite">{run.text || (busy ? '等待正文…' : '本次未收到文本正文。')}</pre>
            {run.truncated && <Alert type="info" message="页面正文达到显示上限，后续内容已省略；完成状态和用量仍按协议记录。" />}
            {run.reasoning && <Collapse items={[{ key: 'reasoning', label: '推理 / 摘要', children: <pre className="playground-output">{run.reasoning}</pre> }]} />}
          </> : <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="配置参数后手动发送，响应与错误会分别显示" />}
        </Card>
        <Collapse items={[{ key: 'preview', label: '请求预览（不含认证头）', children: <><Space wrap><Text code>POST {preview?.path || '/admin/playground/…'}</Text><Button size="small" disabled={!preview} onClick={() => copy(JSON.stringify(preview?.envelope, null, 2))}>复制请求体</Button></Space><pre className="playground-output">{preview ? JSON.stringify(preview.envelope, null, 2) : previewError}</pre></> }]} />
        <Card title="最近测试" size="small" extra={<Button size="small" disabled={!history.length} onClick={() => setHistory([])}>清空记录</Button>}>
          <Text type="secondary">仅本页内存保留最近 10 次参数与状态，离开即清空。载入参数不会自动重发。</Text>
          <Table rowKey="id" size="small" pagination={false} dataSource={history} scroll={{ x: 500 }} locale={{ emptyText: '暂无测试记录' }} columns={[
            { title: '时间', dataIndex: 'at', render: at => new Date(at).toLocaleTimeString() }, { title: '模型 / 协议', render: (_, entry) => <div>{entry.form.model}<div><Text type="secondary">{entry.form.protocol}</Text></div></div> },
            { title: '结果', render: (_, entry) => <Tag color={stateColors[entry.state]}>{stateLabels[entry.state]}{entry.status ? ` · ${entry.status}` : ''}</Tag> },
            { title: '操作', render: (_, entry) => <Button type="link" size="small" disabled={busy || !optionsReady} onClick={() => loadHistory(entry)}>载入参数</Button> },
          ]} />
        </Card>
      </div>
    </div>
  </div>;
}
