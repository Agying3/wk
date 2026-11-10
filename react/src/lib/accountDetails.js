import { accountReason, accountState, stateLabels } from './accountManagement.js';

const missing = '—';
const text = value => typeof value === 'string' && value.trim() ? value : missing;
const number = value => typeof value === 'number' && Number.isFinite(value) ? value.toLocaleString() : '未知';
const bool = value => value === true ? '是' : value === false ? '否' : '未知';
const region = value => ({ cn: '中国区', global: '海外版' }[value] || text(value));
const platform = value => ({ macos: 'macOS', windows: 'Windows' }[value] || text(value));

// The details API uses Unix seconds for credential/credit times and RFC3339 for pool deadlines.
export function accountDetailTimeMillis(value, unit = 'iso') {
  if (unit === 'seconds' && (typeof value !== 'number' || !Number.isFinite(value) || value <= 0)) return null;
  if (unit === 'iso' && (typeof value !== 'string' || !value)) return null;
  const date = new Date(unit === 'seconds' ? value * 1000 : value);
  return Number.isNaN(+date) || date.getUTCFullYear() <= 1 ? null : +date;
}

export function accountDetailTime(value, unit = 'iso') {
  const millis = accountDetailTimeMillis(value, unit);
  return millis === null ? '未知' : new Date(millis).toLocaleString();
}

function rows(entries) {
  return entries.map(([key, label, value]) => ({ key, label, value }));
}

// Explicit fields only: never enumerate a credential document or device profile.
export function accountDetailSections(details, account = {}, fallbackGroups = []) {
  const info = details || {};
  const status = info.status || account;
  const credential = info.credential || {};
  const device = info.device || {};
  const profile = device.profile || {};
  const identifiers = device.identifiers || {};
  const groups = Array.isArray(info.groups) ? info.groups : fallbackGroups;
  const hasCreditSnapshot = accountDetailTimeMillis(status.credit_updated_at, 'seconds') !== null;
  const credit = field => hasCreditSnapshot ? number(status[field]) : '未知';
  const cooldowns = Object.entries(status.model_cooldowns || {}).map(([model, limit]) => `${model}：${text(limit.reason) === missing ? '限流' : text(limit.reason)}；${accountDetailTime(limit.until)}`).join('\n') || missing;
  const source = { stored: '已保存档案', legacy_derived: '未保存 / 兼容推导' }[device.profile_source] || missing;
  return [
    { key: 'basic', title: '基础信息', rows: rows([
      ['nickname', '昵称', text(info.nickname ?? account.nickname)], ['uid', 'UID', text(info.uid ?? account.uid)],
      ['region', '区域', region(info.region ?? account.region)], ['groups', '分组', groups.length ? groups.join('、') : missing],
      ['domain', '账号域名', text(info.domain)], ['enterprise_id', '企业 ID', text(info.enterprise_id)],
      ['account_type', '账号类型', text(info.account_type)], ['oneid_account_id', 'OneID 账号 ID', text(info.oneid_account_id)],
      ['idp', 'IDP', text(info.idp)], ['department_full_name', '部门', text(info.department_full_name)], ['token_type', 'Token 类型', text(info.token_type)],
    ]) },
    { key: 'credential', title: '凭据状态', note: '导入隔离兜底标记仅在账号池状态丢失时生效，不等同于当前禁用状态。', rows: rows([
      ['has_access_token', '已保存 Access Token', bool(credential.has_access_token)],
      ['has_refresh_token', '已保存 Refresh Token', bool(credential.has_refresh_token)],
      ['expires_at', '凭证到期', accountDetailTime(credential.expires_at ?? account.token_expires_at, 'seconds')],
      ['import_disabled', '导入隔离兜底标记', bool(credential.import_disabled)],
    ]) },
    { key: 'device', title: '设备档案', note: device.profile_source === 'legacy_derived' ? '当前展示旧账号的 Windows 兼容推导值，尚未保存设备档案。查看详情不会生成新设备或保存数据。' : '', rows: rows([
      ['profile_source', '档案来源', source], ['platform', '平台', platform(profile.platform)], ['arch', '架构', text(profile.arch)],
      ['os_version', '系统版本', text(profile.os_version)], ['cpu_model', 'CPU', text(profile.cpu_model)],
      ['cpu_cores', 'CPU 核心数', number(profile.cpu_cores)], ['memory_gib', '内存（GiB）', number(profile.memory_gib)],
      ['timezone', '时区', text(profile.timezone)], ['version', '档案版本', number(profile.version)], ['identity_present', '已有设备身份种子', bool(device.identity_present)],
    ]) },
    { key: 'identifiers', title: '设备标识（已脱敏）', rows: rows([
      ['device_identity', '设备身份种子', text(identifiers.device_identity)], ['machine_id', 'Machine ID', text(identifiers.machine_id)],
      ['session_id', 'Session ID', text(identifiers.session_id)], ['cli_session_id', 'CLI Session ID', text(identifiers.cli_session_id)],
      ['web_machine_id', 'Web Machine ID', text(identifiers.web_machine_id)], ['mp_machine_id', '小程序 Machine ID', text(identifiers.mp_machine_id)],
    ]) },
    { key: 'status', title: '运行状态', rows: rows([
      ['state', '状态', typeof status.disabled === 'boolean' ? stateLabels[accountState(status)] : '未知'],
      ['disabled', '当前已禁用', bool(status.disabled)], ['reason', '原因', text(accountReason(status.reason))],
      ['credits', '剩余积分', credit('credits')], ['estimated_spent', '估算已用积分', credit('estimated_spent')], ['effective_credits', '估算可用积分', credit('effective_credits')],
      ['cycle_capacity', '周期积分（已用 / 总量）', `${credit('cycle_capacity_used')} / ${credit('cycle_capacity_size')}`], ['cycle_capacity_remain', '周期剩余积分', credit('cycle_capacity_remain')],
      ['capacity', '累计积分（已用 / 总量）', `${credit('capacity_used')} / ${credit('capacity_size')}`], ['capacity_remain', '累计剩余积分', credit('capacity_remain')],
      ['credit_updated_at', '积分更新时间', accountDetailTime(status.credit_updated_at, 'seconds')],
      ['cool_kind', '冷却类型', text(status.cool_kind)], ['cool_remaining_sec', '冷却剩余秒数', number(status.cool_remaining_sec)],
      ['until', '冷却截止', accountDetailTime(status.until)], ['breaker_until', '熔断截止', accountDetailTime(status.breaker_until)], ['auth_until', '鉴权恢复截止', accountDetailTime(status.auth_until)],
      ['model_cooldowns', '模型限流', cooldowns], ['success_count', '成功请求数', number(status.success_count)], ['err_total', '错误请求数', number(status.err_total)],
      ['in_flight', '在途请求', number(status.in_flight)], ['risk_strikes', '风险计数', number(status.risk_strikes)], ['breaker_fails', '熔断失败数', number(status.breaker_fails)],
      ['last_success', '最近成功', accountDetailTime(status.last_success)], ['last_err', '最近错误', accountDetailTime(status.last_err)],
    ]) },
  ];
}
