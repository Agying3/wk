import React, { useState } from 'react';
import { Alert, App, Button, Form, Input, InputNumber, Modal, Select, Space, Tag, Typography } from 'antd';
import { CopyOutlined, PlusOutlined } from '@ant-design/icons';
import { appendReplacements, buildRewritePrompt, MAX_VARIANTS, parseReplacements } from '../lib/promptVariants';

const { Text } = Typography;

export default function PromptVariantEditor({ form, savedVariants = [], effectiveWeights = [] }) {
  const { message } = App.useApp();
  const [active, setActive] = useState(0);
  const [assistant, setAssistant] = useState(false);
  const [count, setCount] = useState(5);
  const [input, setInput] = useState('');
  const [error, setError] = useState('');
  const variants = Form.useWatch('variants', form) || [];
  const original = Form.useWatch('match', form) || '';
  let prompt = '';
  try { prompt = buildRewritePrompt(original, count); } catch { /* Show the relevant validation when copying. */ }
  const copy = async () => {
    try {
      const text = buildRewritePrompt(original, count);
      try { await navigator.clipboard.writeText(text); message.success('AI 改写提示词已复制'); }
      catch { message.warning('无法访问剪贴板，请手动复制下方提示词'); }
    } catch (e) { message.error(e.message); }
  };
  const importText = () => {
    try {
      const replacements = parseReplacements(input, original);
      const current = form.getFieldValue('variants') || [];
      const next = appendReplacements(current, replacements);
      form.setFieldValue('variants', next);
      setActive(next.length - replacements.length);
      setAssistant(false); setInput(''); setError('');
      message.success(`已填入 ${replacements.length} 个候选，请检查语义后保存规则`);
    } catch (e) { setError(e.message); }
  };
  return <>
    <Form.List name="variants">{(fields, { add, remove }) => {
      const selected = Math.min(active, Math.max(0, fields.length - 1));
      const options = fields.map((field, i) => ({ value: i, label: `候选 ${i + 1} · 权重 ${variants[i]?.weight ?? 1}` }));
      return <section className="pg-variants" aria-label="替换候选编辑器">
        <div className="pg-variant-toolbar"><Text strong>替换候选 <Tag>{fields.length} / {MAX_VARIANTS}</Tag></Text><Space wrap><Button icon={<CopyOutlined />} onClick={() => { setError(''); setAssistant(true); }}>AI 改写助手</Button><Button icon={<PlusOutlined />} disabled={fields.length >= MAX_VARIANTS} onClick={() => { add({ replacement: '', weight: 1 }); setActive(fields.length); }}>添加候选</Button></Space></div>
        <div className="pg-variant-workspace">
          <div className="pg-variant-nav" role="group" aria-label="选择替换候选">{fields.map((field, i) => <button type="button" key={field.key} aria-pressed={i === selected} onClick={() => setActive(i)}><strong>{options[i].label}</strong><span>{variants[i]?.replacement || '空内容 · 删除匹配文本'}</span></button>)}</div>
          <Select className="pg-variant-mobile-select" aria-label="选择替换候选" value={selected} options={options} onChange={setActive} />
          <div className="pg-variant-content">{fields.map((field, i) => {
            const savedIndex = savedVariants.findIndex(v => v.id && v.id === variants[i]?.id);
            return <div key={field.key} hidden={i !== selected}>
              <div className="pg-variant-heading"><Text strong>候选 {i + 1}</Text><Button danger size="small" disabled={fields.length <= 1} onClick={() => { remove(field.name); setActive(Math.max(0, i - 1)); }}>移除当前候选</Button></div>
              <Form.Item name={[field.name, 'id']} hidden><Input /></Form.Item>
              <Form.Item name={[field.name, 'replacement']} label="替换内容" extra="留空会删除匹配内容。请自行确认语义。"><Input.TextArea autoSize={{ minRows: 6, maxRows: 12 }} spellCheck={false} /></Form.Item>
              <Form.Item name={[field.name, 'weight']} label="基础权重" rules={[{ required: true, message: '请输入基础权重' }]} extra="0 表示不分配给新会话；已绑定的会话继续沿用。"><InputNumber min={0} max={1000000} precision={0} aria-label={`候选 ${i + 1} 基础权重`} /></Form.Item>
              {savedIndex >= 0 && <Text type="secondary">上次保存后的新会话占比：{((effectiveWeights[savedIndex] || 0) * 100).toFixed(1)}%</Text>}
            </div>;
          })}</div>
        </div>
      </section>;
    }}</Form.List>
    <Modal title="AI 改写助手" open={assistant} onCancel={() => setAssistant(false)} width={720} footer={<Space wrap><Button onClick={() => setAssistant(false)}>返回编辑</Button><Button type="primary" disabled={!input.trim() || !original.trim()} onClick={importText}>导入到候选</Button></Space>}>
      <div className="pg-rewrite-assistant">
        <Alert type="info" showIcon message="复制提示词给 AI，再粘贴返回的 JSON" description="导入会追加候选，默认权重为 1；已有内容及权重保留，初始空白候选自动填充。请核对原意，保存规则后才生效。" />
        <div className="pg-variant-toolbar"><Space wrap><label htmlFor="pg-rewrite-count">生成数量</label><InputNumber id="pg-rewrite-count" min={1} max={MAX_VARIANTS} precision={0} value={count} onChange={setCount} /></Space><Button icon={<CopyOutlined />} onClick={copy}>复制 AI 提示词</Button></div>
        {!original.trim() && <Alert type="warning" showIcon message="请先在规则中填写匹配原文" />}
        <label htmlFor="pg-rewrite-prompt">提示词预览（可手动复制）</label>
        <Input.TextArea id="pg-rewrite-prompt" value={prompt} readOnly rows={6} spellCheck={false} />
        <label htmlFor="pg-rewrite-result">粘贴 AI 返回的 JSON</label>
        <Input.TextArea id="pg-rewrite-result" value={input} onChange={e => { setInput(e.target.value); setError(''); }} placeholder={'{"replacements":["第一条同义改写","第二条同义改写"]}'} rows={6} spellCheck={false} />
        {error && <Alert type="error" showIcon message={error} />}
        <Text type="secondary">仅在浏览器内生成提示词和解析 JSON，不自动调用 AI。格式和重复检查无法判定语义是否相同。</Text>
      </div>
    </Modal>
  </>;
}
