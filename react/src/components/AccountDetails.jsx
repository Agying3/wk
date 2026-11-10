import React, { useEffect, useState } from 'react';
import { Alert, Button, Descriptions, Space, Typography } from 'antd';
import { accountDetailSections } from '../lib/accountDetails';

const { Text } = Typography;

export default function AccountDetails({ api, uid, account, groups = [], refreshKey = 0 }) {
  const [revision, setRevision] = useState(0);
  const [state, setState] = useState({ uid: null, data: null, loading: true, failed: false });
  useEffect(() => {
    if (!uid) return undefined;
    let disposed = false;
    const controller = new AbortController();
    setState({ uid, data: null, loading: true, failed: false });
    api(`/admin/account/${encodeURIComponent(uid)}/details`, { signal: controller.signal, cache: 'no-store' }).then(body => {
      if (!disposed) {
        if (body?.uid !== uid) throw new Error('Account changed');
        setState({ uid, data: body, loading: false, failed: false });
      }
    }).catch(() => {
      // Do not echo server error bodies: they may not share this endpoint's whitelist.
      if (!disposed) setState({ uid, data: null, loading: false, failed: true });
    });
    return () => { disposed = true; controller.abort(); };
  }, [api, uid, revision, refreshKey]);
  // A changed UID must hide the previous account even before effect cleanup runs.
  const current = state.uid === uid ? state : { data: null, loading: true, failed: false };
  const sections = accountDetailSections(current.data, account, groups);
  return <Space direction="vertical" size={16} style={{ width: '100%' }}>
    <Space wrap><Button size="small" loading={current.loading} onClick={() => setRevision(value => value + 1)}>刷新详情</Button><Text type="secondary">只读本地账号信息，不发起上游请求。</Text></Space>
    {current.loading && <Text type="secondary">正在加载完整详情，先显示现有账号概览。</Text>}
    {current.failed && <Alert type="warning" showIcon message="完整详情加载失败，当前保留账号概览。可点击刷新详情重试。" />}
    {sections.map(section => <div key={section.key} style={{ width: '100%' }}>
      <Typography.Title level={5} style={{ margin: '0 0 8px' }}>{section.title}</Typography.Title>
      {section.note && <div style={{ marginBottom: 8 }}><Text type="secondary">{section.note}</Text></div>}
      <Descriptions column={1} bordered size="small" items={section.rows.map(row => ({ key: row.key, label: row.label, children: row.key === 'uid' ? <Text copyable>{row.value}</Text> : <span style={{ whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }}>{row.value}</span> }))} />
    </div>)}
  </Space>;
}
