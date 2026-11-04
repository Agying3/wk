// 豪猪官方运营商参数表（用户提供，含 2024-10-25 虚拟运营商更新）。
export const haozhumaOperators = [
  { value: '1', label: '移动' },
  { value: '5', label: '联通' },
  { value: '9', label: '电信' },
  { value: '14', label: '广电' },
  { value: '16', label: '虚拟运营商' },
];

// getPhone.ascription，不能套用其它 SDK 的 0/1 卡类型枚举。
export const haozhumaCardTypes = [
  { value: '2', label: '实卡' },
  { value: '1', label: '虚拟号' },
];

// 豪猪官方省份代码表（用户提供，2022-11-26 版本）。
export const haozhumaProvinces = [
  ['11', '北京'], ['12', '天津'], ['13', '河北'], ['14', '山西'], ['15', '内蒙古'],
  ['21', '辽宁'], ['22', '吉林'], ['23', '黑龙江'],
  ['31', '上海'], ['32', '江苏'], ['33', '浙江'], ['34', '安徽'], ['35', '福建'], ['36', '江西'], ['37', '山东'],
  ['41', '河南'], ['42', '湖北'], ['43', '湖南'], ['44', '广东'], ['45', '广西'], ['46', '海南'],
  ['50', '重庆'], ['51', '四川'], ['52', '贵州'], ['53', '云南'], ['54', '西藏'],
  ['61', '陕西'], ['62', '甘肃'], ['63', '青海'], ['64', '宁夏'], ['65', '新疆'],
].map(([value, label]) => ({ value, label }));

export function savedOperatorValues(value) {
  return [...new Set((value || '').split(',').map(s => ({ '2': '5', '3': '9' })[s.trim()] || s.trim()).filter(Boolean))];
}

export function monitorOperatorValue(values) {
  if (values.at(-1) === 'inherit') return null;
  return values.filter(v => v !== 'inherit').join(',');
}
