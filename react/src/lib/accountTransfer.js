export const ACCOUNT_IMPORT_MAX_BYTES = 16 * 1024 * 1024;
export const ACCOUNT_CREDENTIAL_MAX_BYTES = 64 * 1024;

const isObject = value => value !== null && typeof value === 'object' && !Array.isArray(value);
const nonEmptyString = value => typeof value === 'string' && value.trim().length > 0;

export function validTransferPassword(password) {
  const length = new TextEncoder().encode(password).length;
  return length >= 12 && length <= 1024;
}

export function identifyAccountImport(content) {
  if (isObject(content) && content.format === 'workbuddy2api-accounts') {
    if (content.version !== 1 || !nonEmptyString(content.salt) || !nonEmptyString(content.nonce) || !nonEmptyString(content.ciphertext)) {
      throw new Error('加密迁移文件格式或版本无效');
    }
    return { kind: 'encrypted', bundle: content };
  }
  if (isObject(content) && isObject(content.auth) && isObject(content.account)
      && nonEmptyString(content.auth.accessToken) && nonEmptyString(content.account.uid)) {
    return { kind: 'credential', credential: content };
  }
  throw new Error('请选择 WorkBuddy2API 加密迁移文件或单个官方 auth/account 凭据 JSON 文件');
}

export function normalizeAccountImportOptions(options, singleAccount) {
  const groupMode = options.group_mode || 'source';
  if (!['source', 'replace', 'append'].includes(groupMode)) throw new Error('请选择有效的分组策略');
  const normalized = { group_mode: groupMode };
  if (groupMode !== 'source') {
    if (!Array.isArray(options.groups) || options.groups.some(group => typeof group !== 'string' || !group.trim())) {
      throw new Error('请选择有效的目标分组');
    }
    normalized.groups = [...new Set(options.groups.map(group => group.trim()))];
    if (!normalized.groups.length) throw new Error('替换或追加分组时，请至少选择一个目标分组');
  }
  if (options.nickname !== undefined && typeof options.nickname !== 'string') throw new Error('昵称格式无效');
  const nickname = (options.nickname || '').trim();
  if (nickname) {
    if (!singleAccount) throw new Error('只有单账号文件可以自定义昵称');
    if (Array.from(nickname).length > 128 || /[\u0000-\u001f\u007f-\u009f]/u.test(nickname)) {
      throw new Error('昵称最多 128 个字符，且不能包含控制字符');
    }
    normalized.nickname = nickname;
  }
  return normalized;
}

export function buildAccountImportRequest(source, password, uids, options, singleAccount = source?.kind === 'credential') {
  let request;
  if (source?.kind === 'encrypted') {
    if (!validTransferPassword(password)) throw new Error('迁移密码需要 12–1024 字节');
    request = { bundle: source.bundle, password };
  } else if (source?.kind === 'credential') {
    // Preserve all source metadata, including integers outside JS's safe range.
    request = source.credentialJSON === undefined ? { credential: source.credential } : { credential_json: source.credentialJSON };
  } else {
    throw new Error('请先选择账号文件');
  }
  if (options !== undefined) request = { ...request, ...normalizeAccountImportOptions(options, singleAccount) };
  return uids === undefined ? request : { ...request, uids: [...uids] };
}

export function accountImportDeviceLabel(account) {
  if (account.exists || account.device_action === 'skip') return '已存在，跳过设备处理';
  if (account.device_action === 'generate_mac') return '将新建 Mac 设备档案';
  if (account.device_action === 'preserve') {
    if (account.device_platform === 'macos') return '保留已有 Mac 设备档案';
    if (account.device_platform === 'windows') return '保留已有 Windows 设备档案';
    return '保留已有设备档案';
  }
  return '设备信息待确认';
}

// Only the most recently selected file may update the dialog, including errors.
export function createAccountImportFileLoader() {
  let revision = 0;
  return {
    invalidate() { revision++; },
    async load(file) {
      const current = ++revision;
      let result;
      try {
        if (file.size > ACCOUNT_IMPORT_MAX_BYTES) throw new Error('文件超过 16 MiB，请分批导出。');
        let content, rawText;
        try { rawText = await file.text(); content = JSON.parse(rawText); }
        catch (error) {
          // JSON.parse errors can quote credential text. Never display them.
          throw new Error(error instanceof SyntaxError ? '文件不是有效的 JSON 账号文件' : '无法读取账号文件，请重新选择');
        }
        const source = identifyAccountImport(content);
        if (source.kind === 'credential' && file.size > ACCOUNT_CREDENTIAL_MAX_BYTES) throw new Error('单个官方凭据文件超过 64 KiB');
        if (source.kind === 'credential') source.credentialJSON = rawText;
        result = { source, fileName: file.name, error: '' };
      } catch (error) {
        result = { source: null, fileName: file.name, error: error.message };
      }
      return current === revision ? result : null;
    },
  };
}

export function accountImportResultType(result) {
  const rows = result.results || [];
  if (rows.some(row => !row.ok && !row.skipped)) return 'warning';
  return result.ok === true ? 'success' : 'info';
}
