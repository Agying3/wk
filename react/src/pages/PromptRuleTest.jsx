import React, { useEffect, useRef, useState } from 'react';
import { Alert, App, Button, Input, Modal, Select, Space, Table, Tag, Typography } from 'antd';
import { httpDescription, probeExplanation } from '../lib/promptGuard';
import { ruleTestView } from '../lib/promptRuleTest';

const { Text, Paragraph } = Typography;
const stepNames = { control: '中性对照', original: '替换前', replacement: '替换后' };
export default function PromptRuleTest({ api, rule, revision, onClose }) {
  const { message, modal } = App.useApp();
  const [text, setText] = useState(rule.match);
  const [variant, setVariant] = useState(rule.variants?.[0]?.id || '');
  const [models, setModels] = useState([]);
  const [groups, setGroups] = useState([]);
  const [model, setModel] = useState('');
  const [scope, setScope] = useState('');
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState(null);
  const [error, setError] = useState('');
  const abortRef = useRef(null);
  useEffect(() => {
    let live = true;
    Promise.allSettled([api('/v1/models'), api('/admin/groups')]).then(([modelData, groupData]) => {
      if (!live) return;
      if (modelData.status === 'fulfilled') { const items = modelData.value.data || []; setModels(items); setModel(items[0]?.id || ''); }
      if (groupData.status === 'fulfilled') setGroups(groupData.value.groups || []);
      if (modelData.status === 'rejected' || groupData.status === 'rejected') setError('模型或账号分组加载失败，请关闭后重试');
    });
    return () => { live = false; abortRef.current?.abort(); };
  }, [api]);
  const run = async upstream => {
    const controller = new AbortController(); abortRef.current = controller;
    setBusy(true); setError(''); setResult(null);
    try {
      const value = await api('/admin/prompt-guard/rule-test', { method: 'POST', signal: controller.signal, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({
        rule_id: rule.id, variant_id: variant, revision, text, upstream,
        ...(upstream ? { model, account_scope: scope === 'all' ? { kind: 'all' } : { kind: 'group', group: scope.slice(6) } } : {}),
      }) });
      if (!controller.signal.aborted) setResult(value);
    } catch (e) { if (!controller.signal.aborted) setError(e.message); }
    finally { if (abortRef.current === controller) { setBusy(false); abortRef.current = null; } }
  };
  const verify = () => {
    if (!model || !scope) { message.warning('请选择模型和账号范围'); return; }
    modal.confirm({ title: '确认发送规则测试请求？', content: <>
      <Paragraph>模型：{model}；账号范围：{scope === 'all' ? '管理员全池' : `分组 ${scope.slice(6)}`}。</Paragraph>
      <Paragraph>最多 3 次真实请求，可能产生用量：中性对照 → 替换前 → 替换后。固定一个可用账号，使用所选候选，不测试其他规则或候选。对照未通过或请求异常时停止。</Paragraph>
      <Paragraph>将发送你填写的系统提示词及固定的“Reply with OK.”用户消息。结果仅代表本次模型、账号、文本与候选，不判断语义是否相同。</Paragraph>
    </>, okText: '确认发送', cancelText: '取消', onOk: () => { void run(true); } });
  };
  const view = ruleTestView(result);
  return <Modal title={`测试规则：${rule.name || '未命名规则'}`} open onCancel={onClose} width={900} styles={{ body: { maxHeight: '70vh', overflowY: 'auto', paddingRight: 8 } }} footer={<Space wrap>{busy && <Button onClick={() => { abortRef.current?.abort(); setBusy(false); setError('已取消测试；已发送的请求可能仍产生用量'); }}>取消测试</Button>}<Button onClick={onClose}>关闭</Button><Button disabled={busy || !text} onClick={() => run(false)}>本地替换预览</Button><Button type="primary" disabled={busy || !model || !scope || !result?.preview?.matches || result.preview.output === text} onClick={verify}>验证上游效果</Button></Space>}>
    <div className="pg-rule-test">
      <Alert type="info" showIcon message="测试已保存的单条规则及所选候选" description="默认用匹配原文测试，也可粘贴完整系统提示词。本地预览不发送上游请求；其他规则和业务会话选择不会参与。" />
      {!rule.enabled && <Alert type="warning" showIcon message="此规则当前禁用，测试模拟启用后的效果；不会改变启用状态。" />}
      <label htmlFor="pg-test-variant">测试候选</label>
      <Select id="pg-test-variant" disabled={busy} value={variant} onChange={value => { setVariant(value); setResult(null); }} options={rule.variants?.length ? rule.variants.map((item, i) => ({ value: item.id, label: `候选 ${i + 1} · 基础权重 ${item.weight}` })) : [{ value: '', label: '原单候选内容' }]} />
      <label htmlFor="pg-test-text">测试系统提示词</label>
      <Input.TextArea id="pg-test-text" disabled={busy} value={text} onChange={e => { setText(e.target.value); setResult(null); }} rows={6} spellCheck={false} />
      <div className="pg-test-options"><div><label htmlFor="pg-test-model">模型</label><Select id="pg-test-model" showSearch optionFilterProp="label" disabled={busy} value={model || undefined} onChange={value => { setModel(value); setResult(current => current ? { preview: current.preview } : null); }} options={models.map(item => ({ value: item.id, label: item.id }))} placeholder="选择模型" /></div><div><label htmlFor="pg-test-scope">账号范围</label><Select id="pg-test-scope" disabled={busy} value={scope || undefined} onChange={value => { setScope(value); setResult(current => current ? { preview: current.preview } : null); }} placeholder="请选择账号范围" options={[...groups.map(group => ({ value: `group:${group.name}`, label: `分组：${group.name}` })), { value: 'all', label: '管理员全池（明确选择）' }]} /></div></div>
      {busy && <Alert type="info" showIcon message="测试进行中，请等待；可取消测试释放本次账号槽位。" />}
      {error && <Alert type="error" showIcon message={error} />}
      {result?.preview && <><Alert type={result.preview.matches ? 'success' : 'warning'} showIcon message={result.preview.matches ? `命中 ${result.preview.matches} 处${result.preview.output === text ? '，但替换前后内容相同' : ''}` : '未命中原文，请检查大小写、空白与末尾边界'} /><label htmlFor="pg-test-output">替换后的系统提示词</label><Input.TextArea id="pg-test-output" value={result.preview.output} readOnly rows={6} spellCheck={false} /></>}
      {view && <Alert type={view.type} showIcon message={view.title} description="结果仅覆盖本次测试。完整业务请求及其他候选仍可能不同；测试不会解除拦截记录的待处理状态。" />}
      {result?.selection && <Text type="secondary">选号：{result.selection.uid || '未选到账号'} · 范围内 {result.selection.scope_count} 个账号，可用 {result.selection.available_count} 个</Text>}
      {!!result?.trace?.length && <Table size="small" rowKey="step" pagination={false} scroll={{ x: 560 }} dataSource={result.trace} columns={[
        { title: '步骤', dataIndex: 'step', render: value => stepNames[value] },
        { title: '结果', render: (_, row) => <Tag color={row.outcome === 'pass' ? 'green' : 'orange'}>{probeExplanation(row).title}</Tag> },
        { title: 'HTTP', render: (_, row) => httpDescription(row) },
        { title: '耗时', render: (_, row) => `${((row.duration_ms || 0) / 1000).toFixed(2)} 秒` },
      ]} />}
    </div>
  </Modal>;
}
