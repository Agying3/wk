import React from 'react';
import { Alert, Typography } from 'antd';
import { creditCoverage } from '../lib/requestStatistics';

export default function RequestSummaryNotice({ summary }) {
  if (!summary) return null;
  const coverage = summary.coverage;
  const credit = creditCoverage(summary);
  const limits = [];
  if (coverage?.retention_seconds > 0) limits.push(`${(coverage.retention_seconds / 86400).toLocaleString()} 天`);
  if (coverage?.retention_rows > 0) limits.push(`${Number(coverage.retention_rows).toLocaleString()} 条`);
  return (
    <div style={{ marginBottom: 12 }}>
      {coverage?.starts_before_retained && (
        <Alert type="warning" showIcon style={{ marginBottom: 8 }}
          message="所选窗口早于现存日志范围，统计仅基于已保留日志"
          description={`现存日志最早时间：${new Date(coverage.earliest_retained_at * 1000).toLocaleString()}。${limits.length ? `保留限制：${limits.join(' / ')}，任一达到即清理。` : ''}更早的数据是否完整无法确认。`} />
      )}
      <Typography.Text type="secondary">
        {credit
          ? `已知积分：上游 ${credit.upstream.toFixed(4)} / 估算 ${credit.estimated.toFixed(4)}；未知 ${credit.unknown.toLocaleString()} / ${credit.requests.toLocaleString()} 条${credit.percent == null ? '' : `（覆盖 ${credit.percent.toFixed(1)}%）`}。`
          : '当前服务未提供费用来源覆盖率；积分合计可能包含估算值。'}
      </Typography.Text>
    </div>
  );
}
