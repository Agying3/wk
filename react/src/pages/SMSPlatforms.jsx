import React, { useEffect, useState } from 'react';
import { Alert, Button, Card, Input, Select, Space, Typography, message } from 'antd';

const { Text } = Typography;
const DEFAULT_BASE = 'https://api.eomsg.com/zc/data.php';

export default function SMSPlatforms({ settings, running, onSave, onSaved }) {
  const [provider, setProvider] = useState('haozhuma');
  const [keyword, setKeyword] = useState('');
  const [base, setBase] = useState(DEFAULT_BASE);
  const [card, setCard] = useState('全部');
  const [province, setProvince] = useState('');
  const [sid, setSid] = useState('');
  const [token, setToken] = useState('');
  const [fallback, setFallback] = useState('');
  const [saving, setSaving] = useState(false);
  useEffect(() => {
    setProvider(settings.provider || 'haozhuma');
    setKeyword(settings.ecode_keyword || '');
    setSid(settings.sid || '');
  }, [settings.provider, settings.ecode_keyword, settings.sid]);
  useEffect(() => {
    setBase((provider === 'ecode' ? settings.ecode_base : settings.fallback_base) || DEFAULT_BASE);
    setCard((provider === 'ecode' ? settings.ecode_card_type : settings.fallback_card_type) || '全部');
    setProvince((provider === 'ecode' ? settings.ecode_province : settings.fallback_province) || '');
  }, [provider, settings]);
  const changedProvider = provider !== (settings.provider || 'haozhuma');
  const save = async (removeFallback = false) => {
    if (changedProvider && !token.trim()) {
      message.error('切换主平台时请填写该平台的新 Token');
      return;
    }
    if ((provider === 'ecode' || fallback.trim() || settings.fallback_mounted) && !keyword.trim()) {
      message.error('请填写 E码云短信关键词，例如腾讯科技');
      return;
    }
    const patch = { provider, ecode_keyword: keyword.trim() };
    if (provider === 'ecode') {
      Object.assign(patch, { ecode_base: base, ecode_card_type: card, ecode_province: province, fallback: '' });
    } else {
      Object.assign(patch, { sid, fallback_base: base, fallback_card_type: card, fallback_province: province });
      if (fallback.trim()) patch.fallback = fallback.trim();
      if (removeFallback) { patch.fallback = ''; patch.mode = 'auto'; }
    }
    if (changedProvider) { patch.mode = 'auto'; patch.user = ''; patch.pass = ''; }
    if (token.trim()) { patch.token = token.trim(); patch.user = ''; patch.pass = ''; }
    setSaving(true);
    try {
      const result = await onSave(patch);
      if (!result) return;
      setToken(''); setFallback('');
      message.success('接码平台配置已保存');
      onSaved?.();
    } catch (err) { message.error(err.message); }
    finally { setSaving(false); }
  };
  return (
    <Card title="接码平台配置" size="small">
      <Space direction="vertical" style={{ width: '100%' }}>
        <Text type="secondary">任务运行中可切换取号模式；平台凭据和关键词需等任务结束后保存。费用以供应商结算为准。</Text>
        <Space wrap>
          <Select aria-label="主接码平台" value={provider} onChange={setProvider} disabled={running || saving} style={{ width: 150 }}
            options={[{value:'haozhuma',label:'豪猪主用'},{value:'ecode',label:'E码云主用'}]} />
          <Input.Password aria-label="主平台 Token" value={token} onChange={e => setToken(e.target.value)} disabled={running}
            placeholder={changedProvider ? '填写新平台 Token' : '新主平台 Token（留空保留）'} style={{ width: 260 }} />
          {provider === 'haozhuma' && <Input aria-label="豪猪项目 ID" value={sid} onChange={e => setSid(e.target.value)} disabled={running} placeholder="豪猪项目 ID，如 52283" style={{ width: 210 }} />}
        </Space>
        <Space wrap>
          <Input aria-label="E码云短信关键词" value={keyword} onChange={e => setKeyword(e.target.value)} disabled={running} placeholder="E码云短信关键词，如 腾讯科技" style={{ width: 260 }} />
          <Input aria-label="E码云 API 地址" value={base} onChange={e => setBase(e.target.value)} disabled={running} placeholder="E码云 / D1 API 地址" style={{ width: 360 }} />
          <Select aria-label="号码卡类型" value={card} onChange={setCard} disabled={running} style={{ width: 100 }}
            options={['全部','实卡','虚卡'].map(value => ({value,label:value}))} />
          <Input aria-label="号码归属省份" value={province} onChange={e => setProvince(e.target.value)} disabled={running} placeholder="省份（留空不限）" style={{ width: 160 }} />
        </Space>
        {provider === 'haozhuma' && <Input.Password aria-label="E码云兜底 Token" value={fallback} onChange={e => setFallback(e.target.value)} disabled={running}
          placeholder={settings.fallback_mounted ? '新 E码云兜底 Token（留空保留）' : '填写 E码云 Token 后启用双平台'} style={{ maxWidth: 420 }} />}
        <Space>
          <Button onClick={() => save()} loading={saving} disabled={running}>验证并保存</Button>
          {provider === 'haozhuma' && settings.fallback_mounted && <Button danger onClick={() => save(true)} disabled={running || saving}>移除 E码云兜底</Button>}
        </Space>
        {running && <Alert type="info" message="请在上方使用取号模式切换；在途号码继续按原平台收尾。" />}
      </Space>
    </Card>
  );
}

