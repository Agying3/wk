// Wait for the batch so a fast optional/path error cannot hide a concurrent
// authentication failure. The caller retains its serial check for stale loads.
export async function loadDashboardData(api, requestQuery, signal) {
  const results = await Promise.allSettled([
    api('/status', { signal }),
    api('/admin/models', { signal }),
    api(requestQuery, { signal }),
  ]);
  const failures = results.filter(result => result.status === 'rejected').map(result => result.reason);
  const authError = failures.find(error => error?.status === 401);
  if (authError) throw authError;
  const abortError = failures.find(error => error?.name === 'AbortError');
  if (abortError) throw abortError;
  for (const result of results.slice(0, 2)) {
    if (result.status === 'rejected') throw result.reason;
  }
  const requests = results[2].status === 'fulfilled'
    ? results[2].value
    : { data: [], summaryError: results[2].reason };
  return [results[0].value, results[1].value, requests];
}
