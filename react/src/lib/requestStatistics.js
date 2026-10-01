const WINDOW_SECONDS = { '1h': 3600, '6h': 21600, '24h': 86400, '7d': 604800, '30d': 2592000 };

export const windowSeconds = key => WINDOW_SECONDS[key] || 0;

function normalizedFilters(filters) {
  return {
    windowKey: filters.windowKey || '24h',
    modelFilter: filters.modelFilter || 'all',
    statusFilter: filters.statusFilter || 'all',
    routeFilter: filters.routeFilter || 'all',
    accountFilter: filters.accountFilter || 'all',
    regionFilter: filters.regionFilter || 'all',
    ttfbMin: Number(filters.ttfbMin) > 0 ? Math.round(filters.ttfbMin) : null,
    ttfbMax: Number(filters.ttfbMax) > 0 ? Math.round(filters.ttfbMax) : null,
    idSearch: String(filters.idSearch || '').trim(),
  };
}

export const requestFilterKey = filters => JSON.stringify(normalizedFilters(filters));

// Each refresh gets one immutable time range for the request and table filter.
export function buildRequestQuery(filters, { limit = 500, now = Date.now() } = {}) {
  const f = normalizedFilters(filters);
  const until = Math.floor(now / 1000);
  const seconds = windowSeconds(f.windowKey);
  const since = seconds ? until - seconds : 0;
  const params = new URLSearchParams({ limit: String(limit), include: 'summary', until: String(until) });
  if (since) params.set('since', String(since));
  for (const [field, param] of [['modelFilter', 'model'], ['routeFilter', 'route'], ['accountFilter', 'account'], ['regionFilter', 'region']]) {
    if (f[field] !== 'all') params.set(param, f[field]);
  }
  if (f.statusFilter === 'success') params.set('success', '1');
  else if (f.statusFilter === 'failed') params.set('success', '0');
  else if (f.statusFilter !== 'all') params.set('status', f.statusFilter);
  if (f.ttfbMin) params.set('ttfb_min', String(f.ttfbMin));
  if (f.ttfbMax) params.set('ttfb_max', String(f.ttfbMax));
  if (f.idSearch) params.set('id', f.idSearch);
  return { query: params.toString(), since, until, filters: f, filterKey: requestFilterKey(f) };
}

export function hasRequestSummary(summary) {
  return summary != null && typeof summary === 'object'
    && Number.isInteger(summary.requests) && summary.requests >= 0;
}

// An abort is best effort. The serial also rejects servers/mocks that return
// an old response after cancellation, including late errors and loading flags.
export function createLatestRequestLoader(onChange) {
  let serial = 0;
  let controller = null;
  return {
    async load(api, filters, options) {
      const current = ++serial;
      controller?.abort();
      const active = new AbortController();
      controller = active;
      const range = buildRequestQuery(filters, options);
      const empty = { ...range, logs: [], summary: null, error: null, loading: false };
      onChange({ ...empty, loading: true });
      try {
        const body = await api(`/requests?${range.query}`, { signal: active.signal });
        if (current !== serial || active.signal.aborted) return;
        onChange({ ...empty, logs: body.data || [], summary: hasRequestSummary(body.summary) ? body.summary : null,
          error: hasRequestSummary(body.summary) ? null : '当前服务未提供该窗口的汇总，统计暂不可用。' });
      } catch (error) {
        if (current !== serial || active.signal.aborted) return;
        onChange({ ...empty, error: error.message || '请求日志加载失败，请刷新重试。' });
      }
    },
    cancel() {
      ++serial;
      controller?.abort();
    },
  };
}

export function statisticsFromSummary(summary) {
  if (!hasRequestSummary(summary)) return null;
  const n = field => Number(summary[field] || 0);
  const creditBreakdown = Object.hasOwn(summary, 'credits_upstream') && Object.hasOwn(summary, 'credits_estimated');
  return {
    requests: n('requests'), successes: n('successes'), failures: n('failures'),
    inputTokens: n('input_tokens'), outputTokens: n('output_tokens'), totalTokens: n('total_tokens'),
    cacheRead: n('cache_read_tokens'), cacheWrite: n('cache_write_tokens'), toolCalls: n('tool_calls'),
    cacheRate: n('input_tokens') > 0 ? n('cache_read_tokens') / n('input_tokens') * 100 : 0,
    avgTtfb: n('ttfb_samples') > 0 ? n('ttfb_millis_sum') / n('ttfb_samples') : 0,
    avgLatency: n('requests') > 0 ? n('latency_millis_sum') / n('requests') : 0,
    credits: creditBreakdown ? n('credits_upstream') + n('credits_estimated') : n('credits_consumed'),
  };
}

export function creditCoverage(summary) {
  if (!hasRequestSummary(summary) || !Object.hasOwn(summary, 'credit_unknown_requests')) return null;
  const requests = Number(summary.requests);
  const unknown = Number(summary.credit_unknown_requests || 0);
  return { unknown, requests, percent: requests > 0 ? (requests - unknown) / requests * 100 : null,
    upstream: Number(summary.credits_upstream || 0), estimated: Number(summary.credits_estimated || 0) };
}
