import React, { useEffect, useMemo, useRef, useState } from 'react';
import {
  Alert, Button, Card, Divider, Empty, Form, Input, InputNumber, Modal, Popconfirm, Select, Space,
  Switch, Table, Tabs, Tag, Timeline, Tooltip, Typography, message,
} from 'antd';
import { ReloadOutlined, PlusOutlined, ThunderboltOutlined } from '@ant-design/icons';

const { Text, Paragraph } = Typography;

// fmtBytes 订阅流量字节数转可读。
function fmtBytes(n) {
  const v = Number(n || 0);
  if (!v) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  const i = Math.min(units.length - 1, Math.floor(Math.log(v) / Math.log(1024)));
  return `${(v / 1024 ** i).toFixed(i ? 1 : 0)} ${units[i]}`;
}

const PROTO_COLORS = {
  vmess: 'geekblue', vless: 'purple', trojan: 'volcano', ss: 'cyan',
  socks: 'green', http: 'default', wireguard: 'orange',
};

// pgRegionLabel/pgRegionColor 代理分组区域的展示（cn/global/all 三态）。
function pgRegionLabel(r) {
  return r === 'cn' ? 'CN 区' : r === 'global' ? '国际区' : '不限区域';
}
function pgRegionShort(r) {
  return r === 'cn' ? 'CN' : r === 'global' ? '国际' : '不限';
}
function pgRegionColor(r) {
  return r === 'cn' ? 'volcano' : r === 'global' ? 'geekblue' : 'green';
}

// latencyLabel 按当前规则显示延迟：选了真实首字就显示秒，否则显示连通性毫秒。
// 没测过返回空串，调用方自己决定占位。
function latencyLabel(n, cfg) {
  if (cfg?.rules?.latency_source === 'ttfb') {
    if (!n.ttfb_ms || n.ttfb_ms <= 0) return '';
    return `${(n.ttfb_ms / 1000).toFixed(1)}秒`;
  }
  return n.latency_ms >= 0 ? `${n.latency_ms}ms` : '';
}

function nodeStatus(n) {
  if (n.unhealthy) return <Tag color="red">不健康</Tag>;
  if (!n.probed) {
    // 区分"从没测过"和"测了但失败"：失败但未达摘除门槛（3 次）的
    // 显示失败次数，避免误读成"还没轮到测"。
    if (n.fail_streak > 0) return <Tooltip title={`探测失败 ${n.fail_streak} 次（连续 3 次将标记不健康并摘除 10 分钟）`}><Tag color="red">测速失败×{n.fail_streak}</Tag></Tooltip>;
    return <Tag>未测速</Tag>;
  }
  if (n.latency_ms < 0) return <Tag color="orange">超时</Tag>;
  return <Tag color="green">{n.latency_ms} ms</Tag>;
}

/**
 * ReqProxy 请求代理页（#/reqproxy）。
 *
 * 实际请求账号的代理池——订阅 / 节点 / 筛选规则 / 槽位与账号绑定四个维度，
 * 全部经 /admin/reqproxy/* 管理。与「代理池」（注册用 smslogin）完全独立。
 *
 * 数据流：
 *   subscriptions/nodes/slots/config 各自独立拉取；测速与刷新为异步动作
 *   （后端 202 语义：返回 ok 后前端轮询刷新列表）。
 */
export default function ReqProxy({ api }) {
  const [cfg, setCfg] = useState(null);
  const [subs, setSubs] = useState([]);
  const [nodes, setNodes] = useState([]);
  const [slots, setSlots] = useState([]);
  const [proxyGroups, setProxyGroups] = useState([]);
  const [events, setEvents] = useState([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const [subModal, setSubModal] = useState(null); // {mode:'add'|'edit', sub}
  const [importOpen, setImportOpen] = useState(false);
  const [importText, setImportText] = useState('');
  const [importGroup, setImportGroup] = useState('');
  const [pgModal, setPgModal] = useState(null); // {mode:'add'|'edit', group}
  const [pgForm] = Form.useForm();
  // 分组与节点融合视图的状态。
  const [selGroup, setSelGroup] = useState(''); // '' = 全部节点
  const [nodeSearch, setNodeSearch] = useState('');
  const [selNodeKeys, setSelNodeKeys] = useState([]); // 表格勾选的节点 ID
  const [batchMoving, setBatchMoving] = useState(false);
  const [batchTarget, setBatchTarget] = useState(undefined); // 批量挪组目标（受控，挪完复位）
  const [previewOpen, setPreviewOpen] = useState(false);
  const [previewURL, setPreviewURL] = useState('');
  const [previewData, setPreviewData] = useState(null);
  const [previewLoading, setPreviewLoading] = useState(false);
  const [runningJobs, setRunningJobs] = useState({}); // kind → job（刷新后从后端恢复）
  const [form] = Form.useForm();

  const load = async () => {
    setLoading(true);
    try {
      const [config, subList, nodeList, slotList, pgList] = await Promise.all([
        api('/admin/reqproxy/config'),
        api('/admin/reqproxy/subscriptions').catch(() => ({ data: [] })),
        api('/admin/reqproxy/nodes').catch(() => ({ data: [] })),
        api('/admin/reqproxy/slots').catch(() => ({ data: [] })),
        api('/admin/reqproxy/groups').catch(() => ({ data: [] })),
      ]);
      setCfg(config);
      setSubs(subList.data || []);
      setNodes(nodeList.data || []);
      setSlots(slotList.data || []);
      setProxyGroups(pgList.data || []);
      setError('');
    } catch (err) {
      setError(err.message);
    } finally {
      setLoading(false);
    }
  };

  const loadEvents = async () => {
    try {
      const body = await api('/admin/reqproxy/events');
      setEvents((body.data || []).slice().reverse());
    } catch { /* 事件拉取失败不打断主界面 */ }
  };

  // 任务轮询：驱动长动作按钮的 loading。状态在后端，页面刷新后
  // 首轮立即恢复 running 动画，任务完成时弹结果并刷新数据。
  //
  // 乐观登记（optimistic ref）：点击动作时立即记下 job_id 起动画。
  // 轮询响应里：该 id 已 running → 服务端确认，撤乐观标记；
  // 该 id done/error → 弹结果、撤标记；一直查不到（登记竞态/被裁剪）
  // → 30 秒宽限后放弃。UI 的 running = 服务端 running ∪ 乐观标记。
  // 这样晚到的旧响应（不含新任务）不会把动画提前关掉。
  const optimisticRef = useRef({}); // job_id → { kind, at }
  const notifiedRef = useRef({});   // job_id → true（已弹过结果）
  const lastRunningRef = useRef({}); // 上一帧服务端 running（完成检测用）
  // 启动窗口保护：kind → true。按钮点击到 POST 返回、job_id 登记
  // optimisticRef 之间（百毫秒级）并发的 pollJobs 会用服务端快照覆盖
  // runningJobs，把刚点的动画清掉——启动窗口内的 kind 不参与覆盖。
  const startingRef = useRef({});

  const pollJobs = async () => {
    let jobs = [];
    try {
      const body = await api('/admin/reqproxy/jobs?limit=50');
      jobs = body.data || [];
    } catch { /* 拉取失败：保持现状继续轮询 */ }

    const byID = {};
    jobs.forEach(j => { byID[j.id] = j; });

    // 乐观标记结算
    const now = Date.now();
    Object.entries(optimisticRef.current).forEach(([id, info]) => {
      const j = byID[id];
      if (j && j.state === 'running') {
        delete optimisticRef.current[id]; // 服务端已确认，后续由服务端状态接管
      } else if (j && j.state !== 'running') {
        // 完成得比轮询还快：直接弹结果
        if (!notifiedRef.current[id]) {
          notifiedRef.current[id] = true;
          if (j.state === 'error') message.error(`${j.label}失败：${j.note}`);
          else message.success(`${j.label}完成：${j.note}`);
          load();
        }
        delete optimisticRef.current[id];
      } else if (now - info.at > 30000) {
        delete optimisticRef.current[id]; // 宽限超时（登记竞态/列表裁剪）
      }
    });

    // 完成→弹结果（针对服务端确认过的 running 任务）
    const running = {};
    jobs.forEach(j => {
      if (j.state === 'running') running[j.kind] = j;
      else if (lastRunningRef.current[j.id] && !notifiedRef.current[j.id]) {
        notifiedRef.current[j.id] = true;
        if (j.state === 'error') message.error(`${j.label}失败：${j.note}`);
        else message.success(`${j.label}完成：${j.note}`);
        load();
      }
    });
    lastRunningRef.current = running;

    // UI 状态 = 服务端 running ∪ 乐观标记 ∪ 启动窗口保护
    const ui = { ...running };
    Object.values(optimisticRef.current).forEach(info => {
      if (!ui[info.kind]) ui[info.kind] = { kind: info.kind, state: 'running' };
    });
    Object.keys(startingRef.current).forEach(kind => {
      if (!ui[kind]) ui[kind] = { kind, state: 'running' };
    });
    setRunningJobs(ui);

    return Object.keys(ui).length > 0;
  };

  useEffect(() => {
    let stopped = false;
    let timer = null;
    const tick = async () => {
      const hasRunning = await pollJobs();
      if (stopped) return;
      // running/乐观标记期间 1s 高频轮询；空闲 5s 低频兜底（刷新页面后
      // 能在 5 秒内恢复动画）
      timer = setTimeout(tick, hasRunning ? 1000 : 5000);
    };
    tick();
    return () => { stopped = true; clearTimeout(timer); };
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => { load(); loadEvents(); }, []); // eslint-disable-line react-hooks/exhaustive-deps

  // ---- 订阅操作 ----
  const saveSub = async values => {
    try {
      if (subModal.mode === 'add') {
        await api('/admin/reqproxy/subscriptions', {
          method: 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(values),
        });
        message.success('订阅已添加，正在首次拉取…');
        // 后台立即刷新一次拉节点
        const list = await api('/admin/reqproxy/subscriptions');
        const added = (list.data || []).slice(-1)[0];
        if (added) {
          api(`/admin/reqproxy/subscriptions/${added.id}/refresh`, { method: 'POST' })
            .then(() => { message.success('订阅拉取完成'); load(); })
            .catch(err => { message.error(`拉取失败: ${err.message}`); load(); });
        }
      } else {
        await api(`/admin/reqproxy/subscriptions/${subModal.sub.id}`, {
          method: 'PUT', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(values),
        });
        message.success('订阅已更新');
      }
      setSubModal(null);
      load();
    } catch (err) {
      message.error(err.message);
    }
  };

  const refreshSub = async id => {
    startingRef.current['sub-refresh'] = true;
    setRunningJobs(p => ({ ...p, 'sub-refresh': { kind: 'sub-refresh', state: 'running' } }));
    try {
      const body = await api(`/admin/reqproxy/subscriptions/${id}/refresh`, { method: 'POST' });
      delete startingRef.current['sub-refresh'];
      if (body.job_id) {
        optimisticRef.current[body.job_id] = { kind: 'sub-refresh', at: Date.now() };
      }
      pollJobs();
    } catch (err) {
      delete startingRef.current['sub-refresh'];
      setRunningJobs(p => { const q = { ...p }; delete q['sub-refresh']; return q; });
      message.error(`刷新失败: ${err.message}`);
      load();
    }
  };

  const deleteSub = async id => {
    try {
      await api(`/admin/reqproxy/subscriptions/${id}`, { method: 'DELETE' });
      message.success('已删除（其节点已移出池）');
      load();
    } catch (err) {
      message.error(err.message);
    }
  };

  // 整合槽位：同节点多槽合并、删空槽（任务化，动画由 jobs 轮询驱动）
  const compactSlots = async () => {
    startingRef.current.compact = true;
    setRunningJobs(p => ({ ...p, compact: { kind: 'compact', state: 'running' } }));
    try {
      const body = await api('/admin/reqproxy/compact', { method: 'POST' });
      delete startingRef.current.compact;
      if (body.job_id) {
        optimisticRef.current[body.job_id] = { kind: 'compact', at: Date.now() };
      } else {
        message.info('整合已在进行中');
      }
      pollJobs();
    } catch (err) {
      delete startingRef.current.compact;
      setRunningJobs(p => { const q = { ...p }; delete q.compact; return q; });
      message.error(err.message);
    }
  };

  // 重新分配槽位：清空全部槽位/绑定，按当前规则从头分配（任务化）
  const reassignSlots = async () => {
    startingRef.current.reassign = true;
    setRunningJobs(p => ({ ...p, reassign: { kind: 'reassign', state: 'running' } }));
    try {
      const body = await api('/admin/reqproxy/reassign', { method: 'POST' });
      delete startingRef.current.reassign;
      if (body.job_id) {
        optimisticRef.current[body.job_id] = { kind: 'reassign', at: Date.now() };
      } else {
        message.info('重分配已在进行中');
      }
      pollJobs();
    } catch (err) {
      delete startingRef.current.reassign;
      setRunningJobs(p => { const q = { ...p }; delete q.reassign; return q; });
      message.error(err.message);
    }
  };

  const doPreview = async () => {
    if (!previewURL.trim()) return;
    setPreviewLoading(true);
    try {
      const body = await api('/admin/reqproxy/subscriptions/preview', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ url: previewURL.trim() }),
      });
      setPreviewData(body);
    } catch (err) {
      message.error(err.message);
    } finally {
      setPreviewLoading(false);
    }
  };

  // ---- 节点操作 ----
  const doImport = async () => {
    try {
      const body = await api('/admin/reqproxy/nodes/import', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ text: importText, group_id: importGroup || '' }),
      });
      message.success(`已导入 ${body.added} 个节点（归入${importGroup ? '所选分组' : '默认分组'}）`);
      setImportOpen(false);
      setImportText('');
      setImportGroup('');
      load();
    } catch (err) {
      message.error(err.message);
    }
  };

  // ---- 代理分组操作 ----
  const saveProxyGroup = async () => {
    const values = await pgForm.validateFields();
    try {
      if (pgModal.mode === 'add') {
        await api('/admin/reqproxy/groups', {
          method: 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ name: values.name, region: values.region }),
        });
        message.success(`代理分组 ${values.name} 已创建`);
      } else {
        const body = {};
        if (values.name) body.name = values.name;
        if (values.region) body.region = values.region;
        await api(`/admin/reqproxy/groups/${pgModal.group.id}`, {
          method: 'PUT', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        });
        message.success('代理分组已更新');
      }
      setPgModal(null);
      load();
    } catch (err) {
      message.error(err.message);
    }
  };

  const deleteProxyGroup = async g => {
    try {
      await api(`/admin/reqproxy/groups/${g.id}`, { method: 'DELETE' });
      message.success(`代理分组已删除（${g.nodes} 个手动节点回落默认分组）`);
      load();
    } catch (err) {
      message.error(err.message);
    }
  };

  // 手动节点挪组（订阅节点归组由订阅决定，不提供手动挪组）。
  const moveNodeGroup = async (nodeID, groupID) => {
    try {
      await api(`/admin/reqproxy/nodes/${nodeID}/group`, {
        method: 'PUT', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ group_id: groupID || '' }),
      });
      message.success('节点已挪组（相关槽位自动合规调整）');
      load();
    } catch (err) {
      message.error(err.message);
      load();
    }
  };

  // 批量挪组（一次 rebuild，比逐节点调用省 N-1 次全池重建）。
  const batchMoveNodes = async groupID => {
    if (!selNodeKeys.length) return;
    setBatchMoving(true);
    try {
      const body = await api('/admin/reqproxy/nodes/group', {
        method: 'PUT', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ node_ids: selNodeKeys, group_id: groupID || '' }),
      });
      message.success(`已挪动 ${body.moved} 个节点（相关槽位自动合规调整）`);
      setSelNodeKeys([]);
      setBatchTarget(undefined);
      load();
    } catch (err) {
      message.error(err.message);
      setBatchTarget(undefined);
      load();
    } finally {
      setBatchMoving(false);
    }
  };

  const deleteNode = async id => {
    try {
      await api(`/admin/reqproxy/nodes/${id}`, { method: 'DELETE' });
      load();
    } catch (err) {
      message.error(err.message);
    }
  };

  const probeAll = async () => {
    // 乐观动画在请求发出前就位：POST 网络往返期间并发在途的 pollJobs
    // 会用服务端快照覆盖 runningJobs（此刻任务还没登记进列表），
    // startingRef 保护这个窗口。
    startingRef.current['health-run'] = true;
    setRunningJobs(p => ({ ...p, 'health-run': { kind: 'health-run', state: 'running' } }));
    try {
      const body = await api('/admin/reqproxy/health/run', { method: 'POST' });
      delete startingRef.current['health-run'];
      if (body.job_id) {
        optimisticRef.current[body.job_id] = { kind: 'health-run', at: Date.now() };
        message.success('全量测速已开始（结束后自动预热+再平衡）');
      } else {
        // 同类任务已在跑：动画保持，服务端 running 状态下一帧轮询接管
        message.info('全量测速已在进行中');
      }
      pollJobs();
    } catch (err) {
      delete startingRef.current['health-run'];
      setRunningJobs(p => { const q = { ...p }; delete q['health-run']; return q; });
      message.error(err.message);
    }
  };

  const rebalance = async () => {
    startingRef.current.rebalance = true;
    setRunningJobs(p => ({ ...p, rebalance: { kind: 'rebalance', state: 'running' } }));
    try {
      const body = await api('/admin/reqproxy/rebalance', { method: 'POST' });
      delete startingRef.current.rebalance;
      if (body.job_id) {
        optimisticRef.current[body.job_id] = { kind: 'rebalance', at: Date.now() };
      } else {
        message.info('再平衡已在进行中');
      }
      pollJobs();
    } catch (err) {
      delete startingRef.current.rebalance;
      setRunningJobs(p => { const q = { ...p }; delete q.rebalance; return q; });
      message.error(err.message);
    }
  };

  // 单节点测速：后端同步探测（秒级），按钮动画持续到结果返回。
  // 真实首字测速：用指定分组的账号打最小对话，量到第一帧的时间。
  const [ttfbOpen, setTtfbOpen] = useState(false);
  const [ttfbGroup, setTtfbGroup] = useState('');
  const [groups, setGroups] = useState([]);
  const probeTTFB = async () => {
    startingRef.current['ttfb-run'] = true;
    setRunningJobs(p => ({ ...p, 'ttfb-run': { kind: 'ttfb-run', state: 'running' } }));
    setTtfbOpen(false);
    try {
      const body = await api('/admin/reqproxy/ttfb/run', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ group: ttfbGroup, concurrency: 4, timeout_seconds: 60 }),
      });
      delete startingRef.current['ttfb-run'];
      if (body.job_id) {
        optimisticRef.current[body.job_id] = { kind: 'ttfb-run', at: Date.now() };
        message.success('真实首字测速已开始');
      } else {
        message.info('首字测速已在进行中');
      }
      pollJobs();
    } catch (err) {
      delete startingRef.current['ttfb-run'];
      setRunningJobs(p => { const q = { ...p }; delete q['ttfb-run']; return q; });
      message.error(err.message);
    }
  };

  const [probingID, setProbingID] = useState(null);
  const probeOne = async id => {
    setProbingID(id);
    try {
      const body = await api(`/admin/reqproxy/health/run/${id}`, { method: 'POST' });
      if (body && body.ok) {
        message.success(`测速完成：${body.latency_ms} ms`);
      } else {
        message.error(`测速失败：${body?.error || '未知错误'}`);
      }
      load();
    } catch (err) {
      message.error(err.message);
    } finally {
      setProbingID(null);
    }
  };

  // ---- 总开关 ----
  const toggleEnabled = async checked => {
    try {
      await api('/admin/reqproxy/config', {
        method: 'PUT', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ enabled: checked }),
      });
      message.success(checked ? '请求代理已启用：绑定账号的流量将走槽位出口' : '已停用：全部直连');
      load();
    } catch (err) {
      message.error(err.message);
    }
  };

  const subColumns = [
    { title: '名称', dataIndex: 'name', width: 120 },
    {
      title: '区域', dataIndex: 'region', width: 90,
      render: (v, s) => (v
        ? <Tooltip title={`节点归入专属代理分组${s.group_id ? `（${s.group_id}）` : ''}，${v === 'cn' ? '只服务中国区账号' : v === 'global' ? '只服务国际区账号' : '不限区域（两类账号都可用）'}`}>
          <Tag color={pgRegionColor(v)}>{pgRegionShort(v)}</Tag>
        </Tooltip>
        : <Tooltip title="旧订阅未设置区域——编辑补选区域后自动建专属分组"><Tag color="warning">未设置</Tag></Tooltip>),
    },
    {
      title: '订阅地址', dataIndex: 'url', ellipsis: true,
      render: v => <Text code style={{ fontSize: 12 }}>{v}</Text>,
    },
    { title: '刷新间隔', dataIndex: 'interval', width: 90 },
    { title: '自动刷新', dataIndex: 'auto_refresh', width: 90, render: v => (v ? <Tag color="blue">开</Tag> : <Tag>关</Tag>) },
    {
      title: '流量 / 到期', width: 190,
      render: (_, s) => {
        const ui = s.userinfo;
        if (!ui) return <Text type="secondary">-</Text>;
        const used = Number(ui.upload_bytes || 0) + Number(ui.download_bytes || 0);
        const total = Number(ui.total_bytes || 0);
        const pct = total ? Math.min(100, (used / total) * 100) : 0;
        const low = total && pct >= 90;
        return (
          <Space direction="vertical" size={2}>
            {total > 0 && <Text type={low ? 'danger' : 'secondary'}>{fmtBytes(used)} / {fmtBytes(total)}</Text>}
            {ui.expire_at && <Text type="secondary">到期 {new Date(ui.expire_at).toLocaleDateString()}</Text>}
          </Space>
        );
      },
    },
    {
      title: '状态', dataIndex: 'last_status', width: 100,
      render: (v, s) => (v === 'ok'
        ? <Tag color="green">正常</Tag>
        : <Tooltip title={s.last_error}><Tag color={s.last_status ? 'red' : 'default'}>{s.last_status ? '失败' : '未拉取'}</Tag></Tooltip>),
    },
    {
      title: '操作', width: 200,
      render: (_, s) => (
        <Space size={4}>
          <Button size="small" icon={<ReloadOutlined />} loading={!!runningJobs['sub-refresh']} disabled={busy && !runningJobs['sub-refresh']} onClick={() => refreshSub(s.id)}>刷新</Button>
          {guard(<Button size="small" disabled={busy} onClick={() => { setSubModal({ mode: 'edit', sub: s }); form.setFieldsValue(s); }}>编辑</Button>)}
          {guard(<Popconfirm title="删除该订阅？其节点将移出池，相关槽位自动换指向。" onConfirm={() => deleteSub(s.id)}>
            <Button size="small" danger disabled={busy}>删除</Button>
          </Popconfirm>)}
        </Space>
      ),
    },
  ];

  // ---- 分组与节点融合视图 ----
  // groupStats: 每个分组的节点/健康聚合（客户端算，接口已有 unhealthy/probed）。
  const groupStats = useMemo(() => {
    const st = {};
    const agg = id => st[id] || (st[id] = { total: 0, healthy: 0, unhealthy: 0, unprobed: 0 });
    for (const n of nodes) {
      if (!n.group_id) { agg('__ungrouped__'); continue; }
      const a = agg(n.group_id);
      a.total++;
      if (n.unhealthy) a.unhealthy++;
      else if (n.probed) a.healthy++;
      else a.unprobed++;
    }
    return st;
  }, [nodes]);

  // 侧栏列表：全部 + 各分组（默认组置顶）+ 临时"未分组"项（旧数据窗口期）。
  const sidebarGroups = useMemo(() => {
    const list = [...proxyGroups];
    list.sort((a, b) => (b.is_default ? 1 : 0) - (a.is_default ? 1 : 0));
    return list;
  }, [proxyGroups]);
  const ungroupedCount = groupStats.__ungrouped__?.total || 0;

  // 当前视图的节点列表：按选中分组过滤 + 名称搜索。
  const visibleNodes = useMemo(() => {
    let list = nodes;
    if (selGroup === '__ungrouped__') list = nodes.filter(n => !n.group_id);
    else if (selGroup) list = nodes.filter(n => n.group_id === selGroup);
    const kw = nodeSearch.trim().toLowerCase();
    if (kw) list = list.filter(n => (n.name || '').toLowerCase().includes(kw));
    return list;
  }, [nodes, selGroup, nodeSearch]);

  const selGroupObj = proxyGroups.find(g => g.id === selGroup);
  // 选中分组是否可收纳节点（手动组 + 默认组可以；订阅专属组由订阅决定）。
  const selGroupAcceptsNodes = selGroup && selGroupObj && !selGroupObj.sub_id;

  // 侧栏单个分组的健康摘要行。
  const statLine = id => {
    const s = groupStats[id];
    if (!s || !s.total) return <Text type="secondary" style={{ fontSize: 12 }}>0 节点</Text>;
    const bits = [];
    if (s.healthy) bits.push(<Text key="h" type="success" style={{ fontSize: 12 }}>●{s.healthy}</Text>);
    if (s.unhealthy) bits.push(<Text key="u" type="danger" style={{ fontSize: 12 }}>▲{s.unhealthy}</Text>);
    if (s.unprobed) bits.push(<Text key="p" type="secondary" style={{ fontSize: 12 }}>○{s.unprobed}</Text>);
    return (
      <Space size={6} style={{ fontSize: 12 }}>
        <Text strong style={{ fontSize: 12 }}>{s.total} 节点</Text>
        {bits}
      </Space>
    );
  };

  const nodeGroupCol = {
    title: '代理分组', dataIndex: 'group_id', width: 150,
    render: (v, n) => {
      const g = proxyGroups.find(pg => pg.id === v);
      if (!v) return <Tooltip title="未分组（旧数据）——刷新后将自动归入默认分组"><Text type="secondary">未分组</Text></Tooltip>;
      if (!g) return <Tag color="warning">分组已删</Tag>;
      return (
        <Tooltip title={g.sub_id ? '订阅专属分组（随订阅管理）' : g.is_default ? '默认分组（不限区域，对两类账号都可用）' : '手动分组'}>
          <Tag color={pgRegionColor(g.region)}>{g.name}{g.sub_id ? ' · 订阅' : ''}</Tag>
        </Tooltip>
      );
    },
  };

  const nodeColumns = [
    { title: '节点名', dataIndex: 'name', ellipsis: true, render: v => <Text strong>{v}</Text> },
    { title: '协议', dataIndex: 'protocol', width: 90, render: v => <Tag color={PROTO_COLORS[v] || 'default'}>{v}</Tag> },
    { title: '地区', dataIndex: 'region', width: 70, render: v => (v === 'other' ? '-' : v) },
    { title: '来源', dataIndex: 'source', width: 110, ellipsis: true, render: v => (v === 'manual' ? '手动导入' : v) },
    { title: '被指向槽位', dataIndex: 'pointed_slots', width: 100, render: v => (v ? <Tag color={v > 1 ? 'orange' : 'blue'}>{v}</Tag> : '-') },
    { title: '累计请求', dataIndex: 'requests', width: 100, sorter: (a, b) => (a.requests || 0) - (b.requests || 0), render: v => (Number(v || 0) > 0 ? <Text>{Number(v).toLocaleString()}</Text> : <Text type="secondary">-</Text>) },
    { title: '延迟', width: 110, render: (_, n) => nodeStatus(n) },
    {
      title: '真实首字', width: 120,
      sorter: (a, b) => (a.ttfb_ms ?? 1e12) - (b.ttfb_ms ?? 1e12),
      render: (_, n) => {
        if (n.ttfb_ms == null) return <Text type="secondary">-</Text>;
        if (n.ttfb_ms < 0) return <Tooltip title={n.ttfb_error}><Tag color="red">失败</Tag></Tooltip>;
        const color = n.ttfb_ms < 5000 ? 'green' : n.ttfb_ms < 15000 ? 'orange' : 'red';
        return <Tag color={color}>{(n.ttfb_ms / 1000).toFixed(1)} 秒</Tag>;
      },
    },
    {
      title: '操作', width: 240,
      render: (_, n) => (
        <Space size={4}>
          <Button size="small" loading={probingID === n.id} disabled={busy && probingID !== n.id} onClick={() => probeOne(n.id)}>测速</Button>
          {n.source === 'manual' && (
            <Select
              size="small" style={{ minWidth: 110 }} placeholder="分组"
              disabled={busy} value={n.group_id || undefined}
              options={[
                ...proxyGroups.filter(g => !g.sub_id).map(g => ({ value: g.id, label: `${g.name}（${pgRegionShort(g.region)}）` })),
              ]}
              onChange={v => moveNodeGroup(n.id, v || '')}
            />
          )}
          {n.source === 'manual' && (
            guard(<Popconfirm title="删除该节点？" onConfirm={() => deleteNode(n.id)}>
              <Button size="small" danger disabled={busy}>删除</Button>
            </Popconfirm>)
          )}
        </Space>
      ),
    },
  ];

  const slotColumns = [
    { title: '槽位', dataIndex: 'id', width: 130, render: v => <Text code>{v}</Text> },
    { title: 'region', dataIndex: 'region', width: 80, render: v => <Tag color={v === 'cn' ? 'blue' : 'purple'}>{v}</Tag> },
    { title: '本地端口', dataIndex: 'port', width: 90, render: v => <Text code>{v}</Text> },
    {
      title: '当前出口节点', width: 200,
      render: (_, s) => (s.node
        ? <Space size={4}><Text strong>{s.node.name}</Text>{s.node.unhealthy ? <Tag color="red">不健康</Tag> : <Tag color="green">{latencyLabel(s.node, cfg)}</Tag>}</Space>
        : <Tag>空槽</Tag>),
    },
    {
      title: '绑定账号', dataIndex: 'accounts', width: 140,
      render: accounts => (accounts?.length ? <Tag color="blue">{accounts.length} 个账号</Tag> : <Text type="secondary">无</Text>),
    },
    {
      title: '操作', width: 150,
      render: (_, s) => (
        <Select
          size="small" style={{ minWidth: 130 }} placeholder="手动换指向"
          disabled={busy}
          showSearch optionFilterProp="label"
          options={nodes.filter(n => !n.unhealthy).map(n => ({ value: n.id, label: `${n.name}（${latencyLabel(n, cfg) || '未测'}）` }))}
          onChange={async nodeID => {
            try {
              await api(`/admin/reqproxy/slots/${s.id}`, {
                method: 'PUT', headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ node_id: nodeID }),
              });
              message.success('槽位已换指向');
              load();
            } catch (err) { message.error(err.message); }
          }}
        />
      ),
    },
  ];

  const enabled = cfg?.enabled === true;

  // 任务互斥：任何长任务（测速/再平衡/整合/重分配/预热/订阅刷新）运行期间，
  // 禁用所有会改动节点池或槽位拓扑的操作——并发跑会互相干扰（测速中途
  // 删节点→探测悬空；重分配中途导入→槽位错乱）。
  const busy = !!(runningJobs['health-run'] || runningJobs.rebalance ||
    runningJobs.compact || runningJobs.reassign || runningJobs.prewarm ||
    runningJobs['sub-refresh'] || runningJobs['ttfb-run'] || probingID);
  const busyLabel = runningJobs['health-run'] ? '全量测速'
    : runningJobs['ttfb-run'] ? '真实首字测速'
    : runningJobs.rebalance ? '负载再平衡'
    : runningJobs.compact ? '整合槽位'
    : runningJobs.reassign ? '重新分配'
    : runningJobs.prewarm ? '账号预热'
    : runningJobs['sub-refresh'] ? '订阅刷新'
    : probingID ? '单节点测速' : '';
  // busy 时禁用并提示哪个任务在跑（Tooltip 在禁用按钮上仍可悬浮）。
  const guard = el => (busy
    ? <Tooltip title={`${busyLabel}进行中，此操作暂不可用`}>{el}</Tooltip>
    : el);

  const tabItems = [
    {
      key: 'subs', label: '订阅管理', children: (
        <Space direction="vertical" size={12} style={{ width: '100%' }}>
          <Space wrap>
            {guard(<Button type="primary" icon={<PlusOutlined />} disabled={busy} onClick={() => { form.resetFields(); setSubModal({ mode: 'add' }); }}>添加订阅</Button>)}
            <Button onClick={() => { setPreviewData(null); setPreviewURL(''); setPreviewOpen(true); }}>预览订阅</Button>
          </Space>
          {subs.length
            ? <Table rowKey="id" columns={subColumns} dataSource={subs} pagination={false} />
            : <Empty description="尚无订阅。添加一个 v2rayN 订阅地址，或到「分组与节点」手动导入。" />}
        </Space>
      ),
    },
    {
      key: 'groupnodes', label: `分组与节点（${proxyGroups.length} / ${nodes.length}）`, children: (
        <Space direction="vertical" size={12} style={{ width: '100%' }}>
          {/* 池级操作条：不属于任何单个分组 */}
          <Space wrap>
            {guard(<Button type="primary" icon={<PlusOutlined />} disabled={busy} onClick={() => { pgForm.resetFields(); setPgModal({ mode: 'add' }); }}>新建分组</Button>)}
            {guard(<Button icon={<PlusOutlined />} disabled={busy} onClick={() => { setImportGroup(selGroupAcceptsNodes ? selGroup : ''); setImportOpen(true); }}>批量导入</Button>)}
            <Button icon={<ThunderboltOutlined />} loading={!!runningJobs['health-run']} disabled={busy && !runningJobs['health-run']} onClick={probeAll}>全量测速</Button>
            <Tooltip title="用指定分组的账号经每个节点发一次最小对话，量到第一个字的时间。不改变节点健康状态。">
              <Button loading={!!runningJobs['ttfb-run']} disabled={busy && !runningJobs['ttfb-run']}
                onClick={async () => {
                  try {
                    const body = await api('/admin/groups');
                    setGroups((body.groups || []).map(g => g.name));
                  } catch { setGroups([]); }
                  setTtfbOpen(true);
                }}>真实首字测速</Button>
            </Tooltip>
            <Button loading={!!runningJobs.rebalance} disabled={busy && !runningJobs.rebalance} onClick={rebalance}>负载再平衡</Button>
            <Tooltip title="合并同节点的重复槽位、删除空槽位。不动节点选择，只收敛拓扑。">
              <Button loading={!!runningJobs.compact} disabled={busy && !runningJobs.compact} onClick={compactSlots}>整合槽位</Button>
            </Tooltip>
            <Popconfirm title="清空全部槽位和绑定，按当前筛选规则从头重新分配账号？"
              description="已有 pin 的账号保持不动；期间请求走直连兜底。"
              okText="重分配" cancelText="取消" onConfirm={reassignSlots}>
              <Button danger loading={!!runningJobs.reassign} disabled={busy && !runningJobs.reassign}>重新分配槽位</Button>
            </Popconfirm>
          </Space>
          <div style={{ display: 'flex', gap: 12, alignItems: 'stretch' }}>
            {/* 分组侧栏：既是筛选器又是导航 */}
            <div style={{ width: 250, flexShrink: 0, borderRight: '1px solid #f0f0f0', paddingRight: 8 }}>
              <Space direction="vertical" size={4} style={{ width: '100%' }}>
                <div
                  onClick={() => { setSelGroup(''); setSelNodeKeys([]); }}
                  style={{ padding: '6px 8px', borderRadius: 6, cursor: 'pointer', background: !selGroup ? '#e6f4ff' : undefined, border: !selGroup ? '1px solid #91caff' : '1px solid transparent' }}
                >
                  <Space size={6} style={{ width: '100%', justifyContent: 'space-between' }}>
                    <Text strong={!selGroup}>全部节点</Text>
                    <Tag>{nodes.length}</Tag>
                  </Space>
                </div>
                {sidebarGroups.map(g => (
                  <div
                    key={g.id}
                    onClick={() => { setSelGroup(g.id); setSelNodeKeys([]); }}
                    style={{ padding: '6px 8px', borderRadius: 6, cursor: 'pointer', background: selGroup === g.id ? '#e6f4ff' : undefined, border: selGroup === g.id ? '1px solid #91caff' : '1px solid transparent' }}
                  >
                    <Space size={6} style={{ width: '100%', justifyContent: 'space-between' }}>
                      <Space size={4}>
                        <Text strong={selGroup === g.id}>{g.name}</Text>
                        {g.is_default && <Tooltip title="系统内置：区域固定不限，不可改名/删除；手动导入不选分组时归入此组"><Tag color="gold" style={{ marginInlineEnd: 0 }}>默认</Tag></Tooltip>}
                        {g.sub_id && <Tooltip title="订阅专属：名称跟随订阅，成员由订阅决定"><Tag color="cyan" style={{ marginInlineEnd: 0 }}>订阅</Tag></Tooltip>}
                      </Space>
                      <Tag color={pgRegionColor(g.region)} style={{ marginInlineEnd: 0 }}>{pgRegionShort(g.region)}</Tag>
                    </Space>
                    <div style={{ marginTop: 2 }}>{statLine(g.id)}</div>
                  </div>
                ))}
                {ungroupedCount > 0 && (
                  <div
                    onClick={() => { setSelGroup('__ungrouped__'); setSelNodeKeys([]); }}
                    style={{ padding: '6px 8px', borderRadius: 6, cursor: 'pointer', background: selGroup === '__ungrouped__' ? '#e6f4ff' : undefined, border: selGroup === '__ungrouped__' ? '1px solid #91caff' : '1px solid transparent' }}
                  >
                    <Space size={6} style={{ width: '100%', justifyContent: 'space-between' }}>
                      <Text type="warning">未分组（旧数据）</Text>
                      <Tag color="warning">{ungroupedCount}</Tag>
                    </Space>
                  </div>
                )}
                <Text type="secondary" style={{ fontSize: 12, paddingLeft: 8 }}>
                  ●可用 ▲不健康 ○未测速<br />区域决定该组节点服务哪类账号
                </Text>
              </Space>
            </div>
            {/* 右侧：分组详情 + 节点表 */}
            <div style={{ flex: 1, minWidth: 0 }}>
              <Space direction="vertical" size={8} style={{ width: '100%' }}>
                {/* 详情头部：选中分组的元信息 + 分组级操作 */}
                {selGroupObj ? (
                  <Card size="small" style={{ background: '#fafafa' }}>
                    <Space direction="vertical" size={4} style={{ width: '100%' }}>
                      <Space wrap size={8}>
                        <Text strong style={{ fontSize: 15 }}>{selGroupObj.name}</Text>
                        <Tag color={pgRegionColor(selGroupObj.region)}>{pgRegionLabel(selGroupObj.region)}</Tag>
                        {selGroupObj.is_default && <Tag color="gold">默认 · 系统内置</Tag>}
                        {selGroupObj.sub_id && <Tag color="cyan">订阅专属 · 随订阅管理</Tag>}
                        {statLine(selGroupObj.id)}
                        {selGroupObj.sub_name && <Text type="secondary">来源订阅：{selGroupObj.sub_name}</Text>}
                        <Text type="secondary">创建于 {selGroupObj.created_at ? new Date(selGroupObj.created_at).toLocaleString() : '-'}</Text>
                        <span style={{ flex: 1 }} />
                        {selGroupAcceptsNodes && guard(<Button size="small" type="primary" ghost icon={<PlusOutlined />} disabled={busy} onClick={() => { setImportGroup(selGroup); setImportOpen(true); }}>导入到此组</Button>)}
                        {!selGroupObj.is_default && !selGroupObj.sub_id && guard(<Button size="small" disabled={busy} onClick={() => { setPgModal({ mode: 'edit', group: selGroupObj }); pgForm.setFieldsValue({ name: selGroupObj.name, region: selGroupObj.region }); }}>编辑</Button>)}
                        {!selGroupObj.is_default && !selGroupObj.sub_id && guard(
                          <Popconfirm title={`删除代理分组 ${selGroupObj.name}？`} description="组内手动节点将回落到默认分组。" onConfirm={() => { deleteProxyGroup(selGroupObj); setSelGroup(''); }}>
                            <Button size="small" danger disabled={busy}>删除</Button>
                          </Popconfirm>,
                        )}
                      </Space>
                      <Text type="secondary" style={{ fontSize: 12 }}>
                        {selGroupObj.is_default
                          ? '系统内置分组：区域固定不限（对两类账号都可用），不可改名/删除/改区域。手动导入不选分组时归入此组。'
                          : selGroupObj.sub_id
                            ? '订阅专属分组：成员由订阅拉取决定，改名/删除请操作对应订阅。'
                            : `手动分组：仅服务${selGroupObj.region === 'cn' ? '中国区' : selGroupObj.region === 'global' ? '国际区' : '中国区+国际区'}账号槽位。`}
                      </Text>
                    </Space>
                  </Card>
                ) : selGroup === '__ungrouped__' ? (
                  <Alert type="warning" showIcon message="未分组节点（旧数据）"
                    description="这些节点来自旧版本数据，刷新/重启后将自动迁移到默认分组（行为不变：默认分组不限区域）。" />
                ) : (
                  <Text type="secondary">全部节点视图。点左侧分组可筛选；勾选节点后可批量挪组。</Text>
                )}
                {/* 工具栏：搜索 + 批量操作 */}
                <Space wrap>
                  <Input.Search
                    size="small" style={{ width: 220 }} placeholder="搜索节点名"
                    allowClear value={nodeSearch} onChange={e => setNodeSearch(e.target.value)}
                  />
                  {selNodeKeys.length > 0 && (
                    <>
                      <Text type="secondary">已选 {selNodeKeys.length} 个节点</Text>
                      <Select
                        size="small" style={{ minWidth: 200 }} placeholder="批量挪到…"
                        loading={batchMoving} disabled={busy} value={batchTarget}
                        options={proxyGroups.filter(g => !g.sub_id && g.id !== selGroup).map(g => ({ value: g.id, label: `${g.name}（${pgRegionShort(g.region)}）` }))}
                        onChange={v => { setBatchTarget(v); batchMoveNodes(v); }}
                      />
                      <Button size="small" onClick={() => setSelNodeKeys([])}>取消选择</Button>
                    </>
                  )}
                </Space>
                {/* 节点表：单组视图隐藏分组列；全部视图保留 */}
                {visibleNodes.length
                  ? <Table
                      rowKey="id" size="small"
                      columns={selGroup ? nodeColumns : [nodeColumns[0], nodeGroupCol, ...nodeColumns.slice(1)]}
                      dataSource={visibleNodes}
                      rowSelection={{
                        selectedRowKeys: selNodeKeys,
                        onChange: keys => setSelNodeKeys(keys),
                        // 订阅节点不能手动挪组：只在可挪（manual 且允许的目标存在）时可勾选
                        getCheckboxProps: n => ({ disabled: n.source !== 'manual' || busy }),
                      }}
                      pagination={{ pageSize: 15, showSizeChanger: false, showTotal: t => `共 ${t} 个` }}
                    />
                  : <Empty description={nodeSearch ? '没有匹配的节点' : '该视图暂无节点'} />}
              </Space>
            </div>
          </div>
        </Space>
      ),
    },
    {
      key: 'rules', label: '筛选规则', children: (
        <RulesTab api={api} cfg={cfg} onSaved={load} nodes={nodes} />
      ),
    },
    {
      key: 'slots', label: `槽位与绑定（${slots.length}）`, children: (
        slots.length
          ? <Table rowKey="id" columns={slotColumns} dataSource={slots} pagination={false} />
          : <Empty description="尚无槽位。启用模块后，账号第一次发请求时会自动分配。" />
      ),
    },
    {
      key: 'events', label: '事件流', children: (
        events.length
          ? <Timeline items={events.slice(0, 50).map(e => ({
              color: e.kind === 'warn' ? 'red' : e.kind === 'slot-switch' ? 'blue' : 'green',
              children: (
                <Space direction="vertical" size={0}>
                  <Text>{e.msg}</Text>
                  <Text type="secondary" style={{ fontSize: 12 }}>{new Date(e.at).toLocaleString()}</Text>
                </Space>
              ),
            }))} />
          : <Empty description="暂无事件" />
      ),
    },
  ];

  if (error) return <Alert type="error" message={error} showIcon />;

  return (
    <Space direction="vertical" size={14} style={{ width: '100%' }}>
      <Card size="small">
        <Space wrap size={16}>
          <Space>
            <Tooltip title={busy ? `${busyLabel}进行中，暂不能切换` : ''}>
              <Switch checked={enabled} disabled={busy} onChange={toggleEnabled} />
            </Tooltip>
            <Text strong>{enabled ? '请求代理：已启用' : '请求代理：已停用（全部直连）'}</Text>
          </Space>
          {cfg && (
            <>
              <Text type="secondary">订阅 {cfg.subscriptions} · 手动节点 {cfg.manual_nodes} · 槽位 {cfg.slots} · 绑定 {cfg.bindings}</Text>
              <Button size="small" icon={<ReloadOutlined />} onClick={() => { load(); loadEvents(); }} loading={loading}>刷新</Button>
            </>
          )}
        </Space>
      </Card>

      {!enabled && subs.length > 0 && (
        <Alert type="info" showIcon message="模块未启用：账号请求全部直连。启用后，绑定账号的流量将走其槽位的代理出口。" />
      )}

      <Tabs items={tabItems} defaultActiveKey="subs" />

      {/* 订阅编辑弹窗 */}
      <Modal
        open={!!subModal} title={subModal?.mode === 'add' ? '添加订阅' : '编辑订阅'}
        onCancel={() => setSubModal(null)} onOk={() => form.submit()} destroyOnClose
      >
        <Form form={form} layout="vertical" onFinish={saveSub}>
          <Form.Item name="name" label="名称" rules={[{ required: true, message: '必填' }]}>
            <Input placeholder="机场A" />
          </Form.Item>
          <Form.Item name="url" label="订阅地址" rules={[{ required: true, message: '必填' }]}>
            <Input placeholder="https://example.com/api/v1/client/subscribe?token=..." />
          </Form.Item>
          <Form.Item
            name="region" label="区域" initialValue={subModal?.mode === 'add' ? undefined : subModal?.sub?.region}
            rules={[{ required: subModal?.mode === 'add', message: '必选' }]}
            extra="订阅拉到的节点将独立成为一个代理分组（1:1）：CN 区节点只服务中国区账号，国际区节点只服务国际区账号，不限区域则两类账号都可用。"
          >
            <Select
              placeholder="选择区域"
              options={[
                { value: 'cn', label: 'CN 区（只服务中国区账号）' },
                { value: 'global', label: '国际区（只服务国际区账号）' },
                { value: 'all', label: '不限区域（两类账号都可用）' },
              ]}
            />
          </Form.Item>
          <Space size={12}>
            <Form.Item name="auto_refresh" label="自动刷新" valuePropName="checked" initialValue={true}>
              <Switch />
            </Form.Item>
            <Form.Item name="interval" label="刷新间隔" initialValue="1h">
              <Select style={{ width: 120 }} options={['15m', '30m', '1h', '3h', '6h', '12h', '24h'].map(v => ({ value: v, label: v }))} />
            </Form.Item>
          </Space>
        </Form>
      </Modal>

      {/* 订阅预览弹窗 */}
      <Modal open={previewOpen} title="预览订阅（不入池）" onCancel={() => setPreviewOpen(false)} footer={null}>
        <Space direction="vertical" size={12} style={{ width: '100%' }}>
          <Space.Compact style={{ width: '100%' }}>
            <Input placeholder="订阅地址" value={previewURL} onChange={e => setPreviewURL(e.target.value)} />
            <Button type="primary" loading={previewLoading} onClick={doPreview}>拉取预览</Button>
          </Space.Compact>
          {previewData && (
            <>
              <Alert type={previewData.total ? 'success' : 'warning'} showIcon
                message={`解析出 ${previewData.total} 个节点${(previewData.warnings || []).length ? `；${previewData.warnings.join('；')}` : ''}`} />
              {previewData.userinfo && (
                <Text type="secondary">
                  流量 {fmtBytes(Number(previewData.userinfo.upload_bytes || 0) + Number(previewData.userinfo.download_bytes || 0))}
                  / {fmtBytes(previewData.userinfo.total_bytes)}
                  {previewData.userinfo.expire_at ? ` · 到期 ${new Date(previewData.userinfo.expire_at).toLocaleDateString()}` : ''}
                </Text>
              )}
              <div style={{ maxHeight: 300, overflow: 'auto' }}>
                {(previewData.nodes || []).map((n, i) => (
                  <div key={i}><Text>{n.name}</Text> <Tag color={PROTO_COLORS[n.protocol]}>{n.protocol}</Tag> {n.region !== 'other' && <Tag>{n.region}</Tag>}</div>
                ))}
              </div>
            </>
          )}
        </Space>
      </Modal>

      {/* 真实首字测速 */}
      <Modal open={ttfbOpen} title="真实首字测速" okText="开始" onOk={probeTTFB} onCancel={() => setTtfbOpen(false)}>
        <Space direction="vertical" size={8} style={{ width: '100%' }}>
          <Text type="secondary">
            经每个节点发一次最小对话（max_tokens 16），量到第一帧内容的时间。
            这会真实消耗所选分组账号的额度，并发 4 路，单节点上限 60 秒。
          </Text>
          <Select style={{ width: '100%' }} value={ttfbGroup} onChange={setTtfbGroup}
            options={[{ value: '', label: '不限分组（全池账号）' }, ...groups.map(g => ({ value: g, label: g }))]} />
        </Space>
      </Modal>

      {/* 手动导入弹窗 */}
      <Modal open={importOpen} title="批量导入节点" onCancel={() => setImportOpen(false)} onOk={doImport} okText="导入">
        <Space direction="vertical" size={8} style={{ width: '100%' }}>
          <Text type="secondary">每行一条：share-link / socks5:// / http:// 或 host:port:user:pass；也可粘贴整段 Base64。</Text>
          <Select
            style={{ width: '100%' }} value={importGroup} onChange={setImportGroup}
            placeholder="归入代理分组（不选 = 默认分组）"
            allowClear
            options={[
              ...proxyGroups.filter(g => !g.sub_id).map(g => ({ value: g.id, label: `${g.name}（${pgRegionShort(g.region)}）` })),
            ]}
          />
          <Input.TextArea rows={8} value={importText} onChange={e => setImportText(e.target.value)}
            placeholder={'vmess://eyJ2IjoiMiIsInBzIjoi6aaW5a2mMDEiLCJhZGQiOiJ4eC5leGFtcGxlLmNvbSIsInBvcnQiOiI0NDMiLCJpZCI6InV1aWQiLCJuZXQiOiJ3cyJ9\ntrojan://pass@xx.example.com:443#东京01'} />
        </Space>
      </Modal>

      {/* 代理分组编辑弹窗 */}
      <Modal
        open={!!pgModal} title={pgModal?.mode === 'add' ? '新建代理分组' : '编辑代理分组'}
        onCancel={() => setPgModal(null)} onOk={saveProxyGroup} okText={pgModal?.mode === 'add' ? '创建' : '保存'}
      >
        <Form form={pgForm} layout="vertical">
          <Form.Item name="name" label="分组名" rules={[{ required: true, message: '必填' }]}>
            <Input placeholder="例如 CN-手动、全球家宽" maxLength={32} />
          </Form.Item>
          <Form.Item
            name="region" label="区域" initialValue="cn"
            rules={[{ required: true, message: '必选' }]}
            extra="改区域会即时影响该组节点的服务范围：区域违规的槽位自动换指向（端口不变）；不限区域 = 对中国区、国际区账号都可用。"
          >
            <Select
              options={[
                { value: 'cn', label: 'CN 区（只服务中国区账号）' },
                { value: 'global', label: '国际区（只服务国际区账号）' },
                { value: 'all', label: '不限区域（两类账号都可用）' },
              ]}
            />
          </Form.Item>
        </Form>
      </Modal>
    </Space>
  );
}

/**
 * RulesTab 筛选规则 Tab：草稿编辑 + 预览模拟 + 应用。
 */
function RulesTab({ api, cfg, onSaved }) {
  const [form] = Form.useForm();
  const [preview, setPreview] = useState(null);
  const [previewing, setPreviewing] = useState(false);
  const [saving, setSaving] = useState(false);
  const [groups, setGroups] = useState([]);
  const [nodeCount, setNodeCount] = useState(0);

  // 分组列表 + 节点数（自动首字测速的分组下拉和一轮耗时估算用）
  useEffect(() => {
    api('/admin/groups').then(b => setGroups((b.groups || []).map(g => g.name || g))).catch(() => {});
    api('/admin/reqproxy/nodes').then(b => setNodeCount((b.data || []).length)).catch(() => {});
  }, [api, cfg]);

  useEffect(() => {
    if (cfg?.rules) {
      form.setFieldsValue({
        include_keywords: (cfg.rules.include_keywords || []).join(','),
        exclude_keywords: (cfg.rules.exclude_keywords || []).join(','),
        max_latency_ms: cfg.rules.max_latency_ms || 0,
        latency_source: cfg.rules.latency_source === 'ttfb' ? 'ttfb' : 'connect',
        accounts_per_node: cfg.rules.accounts_per_node || 0,
        region_cn: (cfg.rules.region_rules?.cn || []).join(','),
        region_global: (cfg.rules.region_rules?.global || []).join(','),
        ttfb_group: cfg.rules.ttfb_group || '',
        ttfb_interval_seconds: cfg.rules.ttfb_interval_seconds ?? 60,
        ttfb_concurrency: cfg.rules.ttfb_concurrency ?? 1,
        ttfb_timeout_seconds: cfg.rules.ttfb_timeout_seconds ?? 60,
        ttfb_model: cfg.rules.ttfb_model || '',
      });
    }
  }, [cfg, form]);

  const draftRules = () => {
    const v = form.getFieldsValue();
    return {
      include_keywords: (v.include_keywords || '').split(',').map(s => s.trim()).filter(Boolean),
      exclude_keywords: (v.exclude_keywords || '').split(',').map(s => s.trim()).filter(Boolean),
      max_latency_ms: Number(v.max_latency_ms || 0),
      latency_source: v.latency_source === 'ttfb' ? 'ttfb' : 'connect',
      accounts_per_node: Number(v.accounts_per_node || 0),
      region_rules: {
        ...(v.region_cn ? { cn: v.region_cn.split(',').map(s => s.trim()).filter(Boolean) } : {}),
        ...(v.region_global ? { global: v.region_global.split(',').map(s => s.trim()).filter(Boolean) } : {}),
      },
      ttfb_group: v.ttfb_group || '',
      ttfb_interval_seconds: Number(v.ttfb_interval_seconds ?? 60),
      ttfb_concurrency: Number(v.ttfb_concurrency ?? 1),
      ttfb_timeout_seconds: Number(v.ttfb_timeout_seconds ?? 60),
      ttfb_model: (v.ttfb_model || '').trim(),
    };
  };

  const doPreview = async () => {
    setPreviewing(true);
    try {
      const body = await api('/admin/reqproxy/rules/preview', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ rules: draftRules() }),
      });
      setPreview(body);
    } catch (err) {
      message.error(err.message);
    } finally {
      setPreviewing(false);
    }
  };

  const apply = async () => {
    setSaving(true);
    try {
      const draft = draftRules();
      await api('/admin/reqproxy/config', {
        method: 'PUT', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ rules: draft }),
      });
      message.success('规则已应用');
      if (draft.ttfb_group && draft.latency_source === 'ttfb' && draft.max_latency_ms > 0 && draft.max_latency_ms < 5000) {
        message.warning(`延迟上限 ${draft.max_latency_ms}ms 将按真实首字比较：首字中位数约 9 秒，多数节点会被排除，请确认上限已调大`);
      }
      onSaved?.();
    } catch (err) {
      message.error(err.message);
    } finally {
      setSaving(false);
    }
  };

  return (
    <Space direction="vertical" size={14} style={{ width: '100%' }}>
      <Card title="规则（草稿，应用前不影响分配）">
        <Form form={form} layout="vertical">
          <Form.Item name="include_keywords" label="名称白名单（逗号分隔；空 = 不过滤）">
            <Input placeholder="香港,日本" />
          </Form.Item>
          <Form.Item name="exclude_keywords" label="名称黑名单（逗号分隔）">
            <Input placeholder="剩余流量,到期,官网,套餐" />
          </Form.Item>
          <Space size={16} wrap>
            <Form.Item name="max_latency_ms" label="延迟上限 ms（0 = 不限）">
              <InputNumber min={0} step={100} style={{ width: 120 }} />
            </Form.Item>
            <Form.Item name="latency_source" label="分配依据的延迟">
              <Select style={{ width: 220 }} options={[
                { value: 'connect', label: '连通性测速（首页 HEAD）' },
                { value: 'ttfb', label: '真实首字测速' },
              ]} />
            </Form.Item>
            <Form.Item name="accounts_per_node" label="共享度：账号/节点（0 = 不限）">
              <InputNumber min={0} step={1} style={{ width: 120 }} />
            </Form.Item>
          </Space>
          <Space size={16} wrap>
            <Form.Item name="region_cn" label="cn 账号允许的节点地区（逗号分隔；空 = 不限）">
              <Input placeholder="HK,JP,SG" style={{ width: 220 }} />
            </Form.Item>
            <Form.Item name="region_global" label="global 账号允许的节点地区">
              <Input placeholder="US" style={{ width: 220 }} />
            </Form.Item>
          </Space>

          <Divider orientation="left" plain>自动真实首字测速</Divider>
          <Form.Item noStyle shouldUpdate={(prev, cur) => prev.ttfb_group !== cur.ttfb_group}>
            {() => {
              const g = form.getFieldValue('ttfb_group');
              const interval = Number(form.getFieldValue('ttfb_interval_seconds') ?? 60);
              const conc = Number(form.getFieldValue('ttfb_concurrency') ?? 1);
              const batches = Math.ceil(nodeCount / Math.max(1, conc));
              const estMin = nodeCount > 0 ? Math.max(1, Math.round(batches * interval / 60)) : 0;
              return (
                <Space direction="vertical" size={6} style={{ width: '100%' }}>
                  <Text type="secondary">
                    选定分组后，周期连通性测速（首页 HEAD）停止，改用该分组的账号按下面的节奏
                    慢慢扫完全池量真实首字。留空 = 关闭（保持连通性测速）。
                    每次探测是一次真实对话（max_tokens 16），消耗分组账号额度。
                  </Text>
                  {g ? (
                    <Text type="secondary">
                      {nodeCount > 0 && <>当前 {nodeCount} 个节点 ≈ 每批 {conc} 个、批间隔 {interval}s，一轮约 {estMin} 分钟。</>}
                      一轮扫完做一次摘除/预热/再平衡。
                    </Text>
                  ) : null}
                </Space>
              );
            }}
          </Form.Item>
          <Space size={16} wrap>
            <Form.Item name="ttfb_group" label="测速分组（空 = 关闭）">
              <Select style={{ width: 200 }} allowClear placeholder="关闭"
                options={[{ value: '', label: '关闭（用连通性测速）' }, ...groups.map(g => ({ value: g, label: g }))]} />
            </Form.Item>
            <Form.Item name="ttfb_interval_seconds" label="批间隔秒（0–3600）">
              <InputNumber min={0} max={3600} step={5} style={{ width: 110 }} />
            </Form.Item>
            <Form.Item name="ttfb_concurrency" label="每批节点数（1–8）">
              <InputNumber min={1} max={8} step={1} style={{ width: 90 }} />
            </Form.Item>
            <Form.Item name="ttfb_timeout_seconds" label="单节点超时秒（5–180）">
              <InputNumber min={5} max={180} step={5} style={{ width: 120 }} />
            </Form.Item>
            <Form.Item name="ttfb_model" label="探测模型（空 = glm-5.3）">
              <Input placeholder="glm-5.3" style={{ width: 180 }} />
            </Form.Item>
          </Space>
          <Space size={16} wrap>
            <Button onClick={doPreview} loading={previewing}>预览模拟结果</Button>
            <Button type="primary" onClick={apply} loading={saving}>应用规则</Button>
          </Space>
        </Form>
      </Card>

      {preview && (
        <Card title="模拟结果">
          <Space direction="vertical" size={6}>
            <Text>全池 <Text strong>{preview.total}</Text> 个节点 → 规则生效后 <Text strong style={{ color: '#1677ff' }}>{preview.qualified}</Text> 个合格</Text>
            <Text type="secondary">
              过滤分布：{Object.entries(preview.filtered_out || {}).map(([k, v]) => `${k}×${v}`).join('，') || '无'}
            </Text>
            <Text type="secondary">
              按来源：{Object.entries(preview.by_source || {}).map(([k, v]) => `${k === 'manual' ? '手动' : k} ${v} 个`).join('；') || '无'}
            </Text>
            <Text type="secondary">
              地区合格数：{Object.entries(preview.regions || {}).map(([k, v]) => `${k} 区 ${v} 个`).join('；') || '无'}
            </Text>
            <Text type="secondary">
              共享度预估：{Object.entries(preview.shared_per_node || {}).map(([k, v]) => `${k} 区约 ${v} 账号/节点`).join('；') || '暂无绑定'}
            </Text>
            {(preview.notes || []).map((n, i) => <Alert key={i} type="warning" showIcon message={n} />)}
          </Space>
        </Card>
      )}
    </Space>
  );
}
