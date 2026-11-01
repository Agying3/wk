export function ruleTestView(result) {
  if (!result) return null;
  if (result.stale) return { type: 'warning', title: '规则已变化，请重新测试' };
  if (result.cancelled) return { type: 'warning', title: '测试已取消或超时，结果不完整' };
  const rows = result.trace || [];
  const control = rows.find(row => row.step === 'control');
  const original = rows.find(row => row.step === 'original');
  const replacement = rows.find(row => row.step === 'replacement');
  if (!control) return null;
  if (control.outcome !== 'pass') return { type: 'warning', title: '中性对照未通过，无法判断规则效果' };
  if (!replacement || replacement.outcome === 'inconclusive') return { type: 'warning', title: '测试未获得完整结果，无法判断规则效果' };
  if (replacement.outcome === 'blocked') return { type: 'warning', title: '替换后仍触发 11128' };
  if (original?.outcome === 'blocked' && replacement.outcome === 'pass') return { type: 'success', title: '本次原文触发 11128，替换后通过' };
  return { type: 'info', title: '替换前后均通过，本次未复现原文拦截' };
}
