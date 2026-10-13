import React, { useCallback, useEffect, useRef, useState } from 'react';
import { Alert, Button, Card, Input, Select, Space, Statistic, Table, Tabs, Tag, Typography } from 'antd';
import { ReloadOutlined } from '@ant-design/icons';

const { Text, Paragraph } = Typography;
const WINDOWS = [{ value: '1m', label: '近 1 分钟' }, { value: '5m', label: '近 5 分钟' },
  { value: '1h', label: '近 1 小时' }, { value: '6h', label: '近 6 小时' },
  { value: '24h', label: '近 24 小时' }, { value: '7d', label: '近 7 天' }, { value: '30d', label: '近 30 天' }];
const DIMENSIONS = [{ key: 'total', label: '全站' }, { key: 'account', label: '账号' },
  { key: 'key', label: 'API key' }, { key: 'account_group', label: '账号分组' },
  { key: 'proxy_group', label: '代理分组' }, { key: 'proxy', label: '单个代理' }];
const integer = value => Number(value || 0).toLocaleString();
const decimal = value => Number(value || 0).toLocaleString(undefined, { maximumFractionDigits: 2 });
const percent = value => value == null ? '—' : `${decimal(value)}%`;
const seconds = value => value == null ? '—' : `${decimal(value / 1000)}s`;

export default function TrafficStatistics({ api, enabled = true, windowKey, onWindowChange }) {
  const [localWindow, setLocalWindow] = useState('24h');
  const selectedWindow = windowKey || localWindow;
  const [dimension, setDimension] = useState('account');
  const [search, setSearch] = useState('');
  const [snapshot, setSnapshot] = useState(null);
  const [error, setError] = useState(null);
  const [loading, setLoading] = useState(false);
  const [refreshAt, setRefreshAt] = useState(0);
  const serial = useRef(0);
  const controller = useRef(null);
  const refresh = useCallback(async () => {
    if (!enabled) return;
    const current = ++serial.current;
    controller.current?.abort();
    const active = new AbortController();
    controller.current = active;
    setLoading(true);
    try {
      const data = await api(`/admin/traffic?window=${encodeURIComponent(selectedWindow)}`, { signal: active.signal });
      if (current !== serial.current || active.signal.aborted) return;
      setSnapshot({ ...data, window: selectedWindow });
      setError(null);
      setRefreshAt(Date.now());
    } catch (err) {
      if (current !== serial.current || active.signal.aborted) return;
      setSnapshot(null);
      setError(err.message || '流量统计加载失败');
    } finally {
      if (current === serial.current) setLoading(false);
    }
  }, [api, enabled, selectedWindow]);

  useEffect(() => {
    refresh();
    const timer = enabled ? setInterval(refresh, 10000) : null;
    return () => { clearInterval(timer); ++serial.current; controller.current?.abort(); };
  }, [refresh, enabled]);

  const data = snapshot?.window === selectedWindow && enabled ? snapshot : null;
  const total = data?.rows?.find(row => row.dimension === 'total');
  const rows = (data?.rows || []).filter(row => row.dimension === dimension
    && `${row.name} ${row.id}`.toLowerCase().includes(search.trim().toLowerCase()));
  const attempts = ['account', 'account_group', 'proxy', 'proxy_group'].includes(dimension);
  const columns = [
    { title: '名称 / ID', key: 'name', fixed: 'left', width: 230, render: (_, row) => <div><Text strong>{row.name || row.id}</Text><br /><Text type="secondary" copyable>{row.id}</Text></div> },
    { title: attempts ? '上游尝试' : '请求数', dataIndex: 'requests', width: 110, render: integer, sorter: (a, b) => a.requests - b.requests },
    { title: '已结束', dataIndex: 'completed', width: 100, render: integer },
    { title: '实时 RPM', dataIndex: 'rpm', width: 115, render: integer, sorter: (a, b) => a.rpm - b.rpm },
    { title: '平均 RPM', dataIndex: 'avg_rpm', width: 115, render: decimal, sorter: (a, b) => a.avg_rpm - b.avg_rpm },
    { title: '当前并发', dataIndex: 'current_concurrency', width: 115, render: integer, sorter: (a, b) => a.current_concurrency - b.current_concurrency },
    { title: '平均并发', dataIndex: 'avg_concurrency', width: 115, render: decimal, sorter: (a, b) => a.avg_concurrency - b.avg_concurrency },
    { title: '峰值并发', dataIndex: 'peak_concurrency', width: 115, render: integer, sorter: (a, b) => a.peak_concurrency - b.peak_concurrency },
    { title: '成功 / 失败', key: 'outcomes', width: 135, render: (_, row) => <>{integer(row.successes)} / <Text type={row.failures ? 'danger' : undefined}>{integer(row.failures)}</Text></> },
    { title: '成功率', dataIndex: 'success_rate', width: 105, render: percent },
    { title: '输入 / 输出 token', key: 'tokens', width: 190, render: (_, row) => `${integer(row.input_tokens)} / ${integer(row.output_tokens)}` },
    { title: '缓存命中率', dataIndex: 'cache_rate', width: 125, render: percent },
    { title: '平均首 token', dataIndex: 'avg_ttfb_ms', width: 125, render: seconds },
    { title: '平均耗时', dataIndex: 'avg_latency_ms', width: 115, render: seconds },
    { title: '已知积分消耗', dataIndex: 'credits_consumed', width: 140, render: value => Number(value || 0).toFixed(4) },
  ];
  const cards = [['实时 RPM', total?.rpm], ['平均 RPM', total?.avg_rpm], ['当前并发', total?.current_concurrency],
    ['平均并发', total?.avg_concurrency], ['峰值并发', total?.peak_concurrency]];

  return <Space direction="vertical" size="middle" style={{ width: '100%', marginTop: 20 }}>
    <Card title="RPM 与并发" extra={<Space wrap>
      {!windowKey && <Select value={selectedWindow} options={WINDOWS} style={{ width: 140 }} onChange={onWindowChange || setLocalWindow} />}
      <Button icon={<ReloadOutlined />} onClick={refresh} loading={loading} disabled={!enabled}>刷新</Button>
    </Space>}>
      {error && <Alert type="error" showIcon message="流量统计暂不可用" description={error} style={{ marginBottom: 16 }} />}
      {data?.partial && <Alert type="info" showIcon message="此窗口的历史尚未完整覆盖" description={`统计从 ${new Date(data.collection_started_at * 1000).toLocaleString()} 开始采集；平均值按实际已采集时段计算，旧日志无法补齐这些维度。`} style={{ marginBottom: 16 }} />}
      {data?.persistence === 'memory' && <Alert type="warning" showIcon message="当前统计保存在内存，服务重启后会重置" style={{ marginBottom: 16 }} />}
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit,minmax(150px,1fr))', gap: 16 }}>
        {cards.map(([title, value]) => <Statistic key={title} title={`全站${title}`} value={value == null ? '—' : decimal(value)} />)}
      </div>
      <Paragraph type="secondary" style={{ marginBottom: 0, marginTop: 16 }}>
        实时 RPM：最近 60 秒请求起点数；平均 RPM：窗口请求数 / 分钟数；平均并发按在途占用时长计算，包含空闲时间。窗口起点按分钟对齐。
        {data && <> 窗口 {new Date(data.effective_since * 1000).toLocaleString()} — {new Date(data.until * 1000).toLocaleString()}。每 10 秒更新。</>}
      </Paragraph>
    </Card>
    <Card title="分维度统计" extra={<Input.Search placeholder="搜索名称或 ID" value={search} onChange={event => setSearch(event.target.value)} allowClear style={{ width: 220 }} />}>
      <Tabs activeKey={dimension} items={DIMENSIONS} onChange={setDimension} />
      <Paragraph type="secondary">
        <Tag color={attempts ? 'orange' : 'blue'}>{attempts ? '模型请求上游尝试，包含换号重试' : '客户端请求，每个请求计一次'}</Tag>
        {dimension === 'account_group' && '账号多组归属时每个组均计入，各组相加可能超过全站。'}
        {['proxy', 'proxy_group'].includes(dimension) && '按发起请求时选定的代理归属；签到、刷新与探测不计入。'}
        {' '}请求数按发起时间归窗，结果和用量按结束时间归窗；跨窗口请求会造成两者不同。
      </Paragraph>
      <Table rowKey={row => `${row.dimension}:${row.id}`} columns={columns} dataSource={rows} loading={loading}
        pagination={{ pageSize: 20, showSizeChanger: true }} scroll={{ x: 2000 }}
        locale={{ emptyText: error ? '统计不可用，请重试' : '此维度暂无数据' }}
        expandable={{ expandedRowRender: row => <Space wrap size="large">
          <Text>总 token：{integer(row.total_tokens)}</Text><Text>缓存读取：{integer(row.cache_read_tokens)}</Text>
          <Text>未命中 / 创建输入：{integer(row.cache_write_tokens)}</Text><Text>工具调用：{integer(row.tool_calls)}</Text>
		  <Text>缓存统计样本：{integer(row.cache_samples)} · 命中率仅按已返回缓存信息的输入计算</Text>
          <Text>上游积分：{Number(row.credits_upstream || 0).toFixed(4)}</Text><Text>估算积分：{Number(row.credits_estimated || 0).toFixed(4)}</Text>
          <Text>积分未知请求：{integer(row.credit_unknown_requests)}</Text>
        </Space> }} />
      {data && <Text type="secondary">更新于 {new Date(refreshAt).toLocaleTimeString()} · 分钟汇总独立保留 {data.retention_days} 天 · 并发持续到响应结束</Text>}
    </Card>
  </Space>;
}
