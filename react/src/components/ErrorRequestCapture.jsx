import React, { useCallback, useEffect, useRef, useState } from 'react';
import { Alert, Button, Card, Modal, Select, Space, Switch, Table, Tag, Typography } from 'antd';

const endpoint = '/admin/request-capture';

export default function ErrorRequestCapture({ api }) {
  const [snapshot, setSnapshot] = useState(null);
  const [codes, setCodes] = useState([]);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [detail, setDetail] = useState(null);
  const alive = useRef(true);
  const generation = useRef(0);
  const detailGeneration = useRef(0);

  const reload = useCallback(async () => {
    const serial = ++generation.current;
    try {
      const data = await api(endpoint);
      if (alive.current && serial === generation.current) {
        setSnapshot(data);
        setError('');
      }
    } catch (e) {
      if (alive.current && serial === generation.current) setError(e.message);
    }
  }, [api]);

  useEffect(() => {
    alive.current = true;
    reload();
    const timer = setInterval(reload, 10000);
    return () => { alive.current = false; ++generation.current; ++detailGeneration.current; clearInterval(timer); };
  }, [reload]);
  useEffect(() => { if (snapshot) setCodes(snapshot.business_codes.map(String)); }, [snapshot?.business_codes?.join(',')]);
  useEffect(() => {
    if (!detail?.data) return undefined;
    const { id, expires_at: expires } = detail.data;
    const timer = setTimeout(() => setDetail(current => current?.data?.id === id ? { error: '记录已过期并清除' } : current), Math.max(0, Date.parse(expires) - Date.now()));
    return () => clearTimeout(timer);
  }, [detail?.data?.id, detail?.data?.expires_at]);

  const update = async (body, method = 'POST') => {
    setBusy(true);
    ++generation.current;
    try {
      await api(endpoint, { method, headers: { 'Content-Type': 'application/json' }, ...(body ? { body: JSON.stringify(body) } : {}) });
      if (method === 'DELETE') { ++detailGeneration.current; setDetail(null); }
      await reload();
    } catch (e) { if (alive.current) setError(e.message); }
    finally { if (alive.current) setBusy(false); }
  };
  const saveCodes = () => {
    const unique = [...new Set(codes.map(String))];
    if (!unique.length || unique.length > 16 || unique.some(code => !/^[1-9]\d{0,7}$/.test(code))) {
      setError('请输入 1–16 个有效的数字业务错误码'); return;
    }
    update({ business_codes: unique.map(Number) });
  };
  const showDetail = async id => {
    const serial = ++detailGeneration.current;
    setDetail({ loading: true });
    try {
      const data = await api(`${endpoint}/${encodeURIComponent(id)}`);
      if (alive.current && serial === detailGeneration.current) setDetail({ data });
    } catch (e) { if (alive.current && serial === detailGeneration.current) setDetail({ error: e.message }); }
  };

  return (
    <Card title="错误请求自动捕获" extra={<Space><Switch checked={snapshot?.enabled === true} disabled={!snapshot || busy} onChange={enabled => update({ enabled })} /><Button onClick={reload}>刷新</Button></Space>}>
      <Space direction="vertical" style={{ width: '100%' }}>
        <Typography.Text type="secondary">只保留命中业务错误码的请求。包含脱敏后的请求内容，限管理员查看；最多20条、总计32 MiB，10分钟后自动清除，重启后丢失。重试成功也会保留此前的失败尝试。</Typography.Text>
        {error && <Alert type="error" showIcon message={error} />}
        <Space wrap>
          <Select mode="tags" aria-label="捕获业务错误码" style={{ minWidth: 250 }} value={codes} onChange={setCodes} tokenSeparators={[',', ' ']} disabled={busy} />
          <Button onClick={saveCodes} loading={busy} disabled={!snapshot}>保存错误码</Button>
          <Button danger onClick={() => update(null, 'DELETE')} disabled={!snapshot || busy}>清空记录</Button>
          <Typography.Text type="secondary">在途采样 {snapshot?.in_flight || 0} · 繁忙时跳过 {snapshot?.skipped_busy || 0} · 容量淘汰 {snapshot?.evicted || 0}</Typography.Text>
        </Space>
        <Table size="small" rowKey="id" dataSource={snapshot?.records || []} pagination={{ pageSize: 5, hideOnSinglePage: true }} scroll={{ x: 650 }} columns={[
          { title: '时间', dataIndex: 'created_at', render: value => new Date(value).toLocaleTimeString() },
          { title: '请求 / 模型', render: (_, row) => <><div>{row.route}</div><Typography.Text type="secondary">{row.model}</Typography.Text></> },
          { title: '命中错误码', dataIndex: 'matched_codes', render: values => values.map(value => <Tag color="red" key={value}>{value}</Tag>) },
          { title: '最终 HTTP', dataIndex: 'http' },
          { title: '上游尝试', dataIndex: 'attempts' },
          { title: '详情', render: (_, row) => <Button type="link" onClick={() => showDetail(row.id)}>查看请求</Button> },
        ]} />
      </Space>
      <Modal open={detail !== null} title="错误请求捕获详情" width={1000} footer={null} onCancel={() => { ++detailGeneration.current; setDetail(null); }} destroyOnHidden>
        {detail?.error && <Alert type="error" message={detail.error} />}
        {detail?.loading && <Typography.Text>正在读取…</Typography.Text>}
        {detail?.data && <pre style={{ maxHeight: '65vh', overflow: 'auto', whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }}>{JSON.stringify(detail.data, null, 2)}</pre>}
      </Modal>
    </Card>
  );
}
