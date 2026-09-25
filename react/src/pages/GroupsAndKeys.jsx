import React, { useCallback, useEffect, useState } from 'react';
import {
  Alert, App, Button, Card, Empty, Form, Input, Modal, Popconfirm, Select,
  Space, Table, Tag, Tooltip, Typography,
} from 'antd';
import { DeleteOutlined, EditOutlined, PlusOutlined, ReloadOutlined, ThunderboltOutlined } from '@ant-design/icons';

const { Text, Paragraph } = Typography;

// 区域标签。
const regionTag = region => {
  if (region === 'cn') return <Tag color="volcano">CN 区</Tag>;
  if (region === 'global') return <Tag color="geekblue">国际区</Tag>;
  return <Tag color="default">不限区域</Tag>;
};
const REGION_OPTIONS = [
  { value: 'cn', label: 'CN 区（只装中国区账号）' },
  { value: 'global', label: '国际区（只装国际区账号）' },
  { value: 'all', label: '不限区域（CN + 国际都可）' },
];
const REGION_EDIT_OPTIONS = REGION_OPTIONS; // 编辑时 all 也允许（回退迁移态）

// GroupsAndKeysPage 分组与密钥管理。
//
// 分组：default 恒存在不可改删（后端兜底），其余可建/改名/改区域/删（组内
// 有账号或密钥引用时后端拒绝删除，界面如实报错）。区域：cn/global/all，
// 区域化后 CN 分组不能装国际账号、不能绑国际代理分组。
// 密钥：创建时完整密钥只在弹窗里出现一次；列表脱敏展示。
export default function GroupsAndKeysPage({ api }) {
  const { message } = App.useApp();
  const [groups, setGroups] = useState(null);
  const [keys, setKeys] = useState(null);
  const [proxyGroups, setProxyGroups] = useState([]);
  const [loading, setLoading] = useState(false);
  // groupModal: {mode:'create'|'rename', name?}
  const [groupModal, setGroupModal] = useState(null);
  const [keyModal, setKeyModal] = useState(null);
  // bindModal: {name} 编辑账号分组的代理分组绑定。
  const [bindModal, setBindModal] = useState(null);
  // createdKey: 新建密钥的一次性完整回显。
  const [createdKey, setCreatedKey] = useState(null);
  const [groupForm] = Form.useForm();
  const [keyForm] = Form.useForm();
  const [bindForm] = Form.useForm();

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const reqs = [api('/admin/groups'), api('/admin/keys')];
      // 代理分组列表可选（reqproxy 模块未启用时 503，绑定编辑器降级隐藏）。
      const results = await Promise.allSettled(reqs);
      setGroups(results[0].status === 'fulfilled' ? results[0].value.groups || [] : null);
      setKeys(results[1].status === 'fulfilled' ? results[1].value.keys || [] : null);
      setProxyGroups([]);
      try {
        const pg = await api('/admin/reqproxy/groups');
        setProxyGroups(pg.data || []);
      } catch { /* 模块未启用 */ }
    } catch (err) {
      message.error(err.message);
    } finally {
      setLoading(false);
    }
  }, [api, message]);

  useEffect(() => { load(); }, [load]);

  const groupName = name => (name === 'default' ? <Space><Tag color="blue">default</Tag><Text type="secondary">默认分组</Text></Space> : <Tag>{name}</Tag>);

  const submitGroup = async () => {
    const values = await groupForm.validateFields();
    try {
      if (groupModal.mode === 'create') {
        await api('/admin/groups', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ name: values.name, region: values.region }) });
        message.success(`分组 ${values.name} 已创建`);
      } else {
        await api(`/admin/groups/${encodeURIComponent(groupModal.name)}`, { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ name: values.name }) });
        message.success('分组已改名（账号归属与密钥绑定已同步迁移）');
      }
      setGroupModal(null);
      await load();
    } catch (err) {
      message.error(err.message);
    }
  };

  const changeRegion = async (name, region) => {
    try {
      await api(`/admin/groups/${encodeURIComponent(name)}`, { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ region }) });
      message.success(`分组 ${name} 区域已更新`);
      await load();
    } catch (err) {
      message.error(err.message);
      await load(); // 刷新回显真实值
    }
  };

  const autoDetect = async () => {
    try {
      const res = await api('/admin/groups/auto-detect-regions', { method: 'POST' });
      const set = (res.results || []).filter(r => r.action === 'set').length;
      const skipped = (res.results || []).length - set;
      message.success(`区域识别完成：${set} 个分组已设置${skipped ? `，${skipped} 个跳过（混合区域或已设置）` : ''}`);
      await load();
    } catch (err) {
      message.error(err.message);
    }
  };

  const deleteGroup = async name => {
    try {
      await api(`/admin/groups/${encodeURIComponent(name)}`, { method: 'DELETE' });
      message.success(`分组 ${name} 已删除`);
      await load();
    } catch (err) {
      message.error(err.message);
    }
  };

  const submitBind = async () => {
    const values = await bindForm.validateFields();
    try {
      await api(`/admin/groups/${encodeURIComponent(bindModal.name)}/proxy-bindings`, {
        method: 'PUT', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ proxy_groups: values.proxy_groups || [] }),
      });
      message.success('代理分组绑定已更新（已绑定的账号下次分配代理时生效）');
      setBindModal(null);
      await load();
    } catch (err) {
      message.error(err.message);
    }
  };

  const submitKey = async () => {
    const values = await keyForm.validateFields();
    try {
      const created = await api('/admin/keys', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ name: values.name || '', group: values.group }) });
      setKeyModal(null);
      setCreatedKey(created);
      await load();
    } catch (err) {
      message.error(err.message);
    }
  };

  const editKey = record => {
    Modal.confirm({
      title: `改绑密钥 ${record.key}`,
      content: (
        <div style={{ marginTop: 12 }}>
          <Paragraph type="secondary">该密钥只能使用所选分组里的账号。</Paragraph>
          <Input defaultValue={record.name} id="key-name-input" placeholder="备注名（可选）" style={{ marginBottom: 8 }} />
          <Input defaultValue={record.group || 'default'} id="key-group-input" placeholder="绑定分组" />
        </div>
      ),
      okText: '保存',
      cancelText: '取消',
      onOk: async () => {
        const name = document.getElementById('key-name-input').value.trim();
        const group = document.getElementById('key-group-input').value.trim() || 'default';
        try {
          await api(`/admin/keys/${encodeURIComponent(record.id)}`, { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ name, group }) });
          message.success('密钥已更新');
          await load();
        } catch (err) {
          message.error(err.message);
          throw err; // 阻止弹窗关闭，让用户看到错误
        }
      },
    });
  };

  const deleteKey = async record => {
    try {
      await api(`/admin/keys/${encodeURIComponent(record.id)}`, { method: 'DELETE' });
      message.success('密钥已删除（立即失效）');
      await load();
    } catch (err) {
      message.error(err.message);
    }
  };

  if (groups === null) {
    return <Card loading />;
  }
  if (!groups.length) {
    return <Empty description="分组存储未启用" />;
  }

  const groupColumns = [
    { title: '分组', dataIndex: 'name', render: groupName },
    { title: '区域', dataIndex: 'region', width: 130, render: regionTag },
    { title: '账号数', dataIndex: 'accounts', render: v => <Text>{v}</Text> },
    { title: '绑定密钥', dataIndex: 'keys', render: v => (v ? <Tag color="geekblue">{v} 把</Tag> : <Text type="secondary">无</Text>) },
    {
      title: '绑定代理分组',
      dataIndex: 'proxy_bindings',
      render: (v, record) => (v
        ? <Tooltip title="该分组的账号只能走这些代理分组的节点（所有归属分组都绑定时生效）"><Tag color="cyan">{v} 个</Tag></Tooltip>
        : <Text type="secondary">不限制</Text>),
    },
    {
      title: '操作',
      render: (_, record) => (
        record.default
          ? <Tooltip title="默认分组不可改名或删除（区域与代理绑定可改）"><Space><Text type="secondary">—</Text></Space></Tooltip>
          : (
            <Space>
              <Button size="small" icon={<EditOutlined />} onClick={() => { setGroupModal({ mode: 'rename', name: record.name }); groupForm.setFieldsValue({ name: record.name }); }}>改名</Button>
              <Popconfirm title={`删除分组 ${record.name}？`} description="组内还有账号（唯一归属）或密钥时会拒绝。" onConfirm={() => deleteGroup(record.name)}>
                <Button size="small" danger icon={<DeleteOutlined />}>删除</Button>
              </Popconfirm>
            </Space>
          )
      ),
    },
  ];

  const keyColumns = [
    { title: '密钥', dataIndex: 'key', render: v => <Text code copyable={{ text: v }}>{v}</Text> },
    { title: '备注', dataIndex: 'name', render: v => v || <Text type="secondary">-</Text> },
    { title: '分组', dataIndex: 'group', render: v => <Tag color={v === 'default' ? 'blue' : 'purple'}>{v || '不限'}</Tag> },
    { title: '创建时间', dataIndex: 'created_at', render: v => (v ? new Date(v).toLocaleString() : '-') },
    {
      title: '操作',
      render: (_, record) => (
        <Space>
          <Button size="small" icon={<EditOutlined />} onClick={() => editKey(record)}>改绑</Button>
          <Popconfirm title="删除这把密钥？" description="删除后立即失效，用它的调用会全部 401。" onConfirm={() => deleteKey(record)}>
            <Button size="small" danger icon={<DeleteOutlined />}>删除</Button>
          </Popconfirm>
        </Space>
      ),
    },
  ];

  // 代理分组绑定选项（区域兼容过滤：账号分组为 all 时都可，否则同区）。
  const bindOptions = record => proxyGroups
    .filter(pg => !record.region || record.region === 'all' || pg.region === record.region)
    .map(pg => ({
      value: pg.id,
      label: `${pg.name}（${pg.region === 'cn' ? 'CN' : '国际'}区 · ${pg.nodes} 节点${pg.sub_id ? ' · 订阅专属' : ''}）`,
    }));

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <Card
        title="账号分组"
        extra={(
          <Space>
            <Button icon={<ReloadOutlined />} loading={loading} onClick={load}>刷新</Button>
            <Tooltip title="按组内成员账号的实际区域自动设置：纯 CN 账号组→CN 区、纯国际组→国际区；混合或空组跳过。仅处理「不限区域」的分组。">
              <Button icon={<ThunderboltOutlined />} onClick={autoDetect}>区域识别</Button>
            </Tooltip>
            <Button type="primary" icon={<PlusOutlined />} onClick={() => { setGroupModal({ mode: 'create' }); groupForm.resetFields(); }}>新建分组</Button>
          </Space>
        )}
      >
        <Paragraph type="secondary">
          账号可属于多个分组（在账号列表里设置）；密钥绑定一个分组后只能使用该分组的账号。
          区域化后：CN 区分组不能装国际区账号，反之亦然；「不限区域」为迁移过渡态，兼容两类账号。
        </Paragraph>
        <Table
          rowKey="name" size="small" pagination={false} columns={groupColumns} dataSource={groups}
          expandable={{
            expandedRowRender: record => (
              <Space direction="vertical" size={8} style={{ width: '100%' }}>
                <Space wrap>
                  <Text type="secondary">区域：</Text>
                  <Select
                    size="small" defaultValue={record.region} style={{ minWidth: 220 }}
                    options={REGION_EDIT_OPTIONS} onChange={v => changeRegion(record.name, v)}
                  />
                  <Button size="small" onClick={() => { setBindModal({ name: record.name }); bindForm.setFieldsValue({ proxy_groups: record.proxy_binding_ids || [] }); }}>
                    绑定代理分组{record.proxy_bindings ? `（${record.proxy_bindings}）` : ''}
                  </Button>
                </Space>
                <Text type="secondary" style={{ fontSize: 12 }}>
                  改区域后组内已有账号不会自动移出；如提示区域不匹配，请先到账号列表调整。绑定变更不会自动迁移账号的代理槽位——需要时在「请求代理」页点「重新分配槽位」。
                </Text>
              </Space>
            ),
            rowExpandable: () => true,
          }}
        />
      </Card>

      <Card
        title="API 密钥"
        extra={<Button type="primary" icon={<PlusOutlined />} onClick={() => { setKeyModal(true); keyForm.resetFields(); keyForm.setFieldsValue({ group: 'default' }); }}>新建密钥</Button>}
      >
        <Paragraph type="secondary">分组密钥只能使用绑定分组的账号；config 里的管理员密钥（api_key）不受分组限制，也不会出现在此列表。</Paragraph>
        {keys.length
          ? <Table rowKey="id" size="small" pagination={false} columns={keyColumns} dataSource={keys} />
          : <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="还没有创建密钥" />}
      </Card>

      <Modal
        open={!!groupModal}
        title={groupModal?.mode === 'create' ? '新建分组' : `改名分组 ${groupModal?.name}`}
        onOk={submitGroup}
        onCancel={() => setGroupModal(null)}
        okText={groupModal?.mode === 'create' ? '创建' : '改名'}
        cancelText="取消"
      >
        <Form form={groupForm} layout="vertical">
          <Form.Item
            name="name"
            label="分组名"
            rules={[
              { required: true, message: '请输入分组名' },
              { pattern: /^[A-Za-z0-9\-_.]{1,32}$/, message: '字母、数字与 - _ .，最多 32 字符' },
            ]}
          >
            <Input placeholder="例如 vip、cn-pool" maxLength={32} />
          </Form.Item>
          {groupModal?.mode === 'create' && (
            <Form.Item
              name="region" label="区域" initialValue="cn"
              rules={[{ required: true, message: '请选择区域' }]}
              extra="CN 区分组只能加入中国区账号；国际区分组只能加入国际区账号。"
            >
              <Select options={REGION_OPTIONS.slice(0, 2)} />
            </Form.Item>
          )}
        </Form>
      </Modal>

      <Modal
        open={!!bindModal}
        title={`分组「${bindModal?.name}」绑定的代理分组`}
        onOk={submitBind}
        onCancel={() => setBindModal(null)}
        okText="保存"
        cancelText="取消"
      >
        {proxyGroups.length === 0
          ? <Alert type="info" showIcon message="没有可绑定的代理分组" description="请先在「请求代理」页创建订阅（自动建专属分组）或手动分组。" style={{ marginBottom: 12 }} />
          : (
            <Form form={bindForm} layout="vertical">
              <Form.Item
                name="proxy_groups" label="允许的代理分组（多选）"
                extra="留空 = 不限制（该分组账号可用全部节点）。绑定的分组全部生效后，账号只能走这些分组里的节点；与其他分组的绑定取并集。"
              >
                <Select mode="multiple" placeholder="不限制" options={bindOptions(groups.find(g => g.name === bindModal?.name) || { region: 'all' })} />
              </Form.Item>
            </Form>
          )}
      </Modal>

      <Modal
        open={!!keyModal}
        title="新建 API 密钥"
        onOk={submitKey}
        onCancel={() => setKeyModal(null)}
        okText="创建"
        cancelText="取消"
      >
        <Form form={keyForm} layout="vertical">
          <Form.Item name="name" label="备注名（可选）">
            <Input placeholder="例如：给 NewAPI 的中转密钥" maxLength={64} />
          </Form.Item>
          <Form.Item name="group" label="绑定分组" rules={[{ required: true, message: '请选择分组' }]}>
            <Select
              options={groups.map(g => ({ value: g.name, label: `${g.name}${g.region && g.region !== 'all' ? `（${g.region === 'cn' ? 'CN' : '国际'}区）` : ''}` }))}
              placeholder="default"
            />
          </Form.Item>
        </Form>
      </Modal>

      <Modal
        open={!!createdKey}
        title="密钥已创建"
        onOk={() => setCreatedKey(null)}
        onCancel={() => setCreatedKey(null)}
        okText="我已保存"
        cancelButtonProps={{ style: { display: 'none' } }}
      >
        <Alert
          type="warning"
          showIcon
          message="完整密钥仅此一次展示"
          description="关闭后无法再次查看，请立即复制保存到你的调用方配置里。"
        />
        <Paragraph style={{ marginTop: 12 }}>
          <Text code copyable={{ text: createdKey?.key }}>{createdKey?.key}</Text>
        </Paragraph>
        <Space>
          <Tag color={createdKey?.group === 'default' ? 'blue' : 'purple'}>{createdKey?.group}</Tag>
          {createdKey?.name && <Text type="secondary">{createdKey.name}</Text>}
        </Space>
      </Modal>
    </Space>
  );
}
