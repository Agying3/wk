import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import { buildRequestQuery, createLatestRequestLoader, creditCoverage, hasRequestSummary, requestFilterKey, statisticsFromSummary } from './requestStatistics.js';

const summary = (tokens = 100) => ({ requests: 1, successes: 1, failures: 0, input_tokens: tokens,
  output_tokens: 5, total_tokens: tokens + 5, cache_read_tokens: tokens / 2,
  ttfb_millis_sum: 1200, ttfb_samples: 1, latency_millis_sum: 5000 });
function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

test('manual refresh advances both bounds while keeping the same filter identity', async () => {
  const states = [], urls = [];
  const loader = createLatestRequestLoader(value => states.push(value));
  const api = async url => { urls.push(new URLSearchParams(url.split('?')[1])); return {data: [], summary:summary()}; };
  const filters = { windowKey: '1h', modelFilter: 'model-a' };
  await loader.load(api, filters, {now: 7200000});
  await loader.load(api, filters, {now: 9000000});
  assert.equal(urls[0].get('since'), '3600');
  assert.equal(urls[1].get('since'), '5400');
  assert.equal(urls[1].get('until'), '9000');
  assert.equal(states.at(-1).since, 5400);
  assert.equal(states.at(-1).until, 9000);
  assert.equal(states.at(-1).filterKey, requestFilterKey(filters));
});

test('a late old response cannot overwrite the selected model', async () => {
  const a = deferred(), b = deferred(), states = [], signals = [];
  const loader = createLatestRequestLoader(value => states.push(value));
  const api = (url, {signal}) => { signals.push(signal); return url.includes('model=model-a') ? a.promise : b.promise; };
  const pa = loader.load(api, {modelFilter:'model-a'});
  const pb = loader.load(api, {modelFilter:'model-b'});
  b.resolve({data:[{model:'model-b'}],summary:summary(200)});
  await pb;
  const count = states.length;
  a.resolve({data:[{model:'model-a'}],summary:summary(100)});
  await pa;
  assert.equal(signals[0].aborted, true);
  assert.equal(states.length, count);
  assert.equal(states.at(-1).logs[0].model, 'model-b');
  assert.equal(statisticsFromSummary(states.at(-1).summary).inputTokens, 200);
});

test('an old completion does not clear loading for a new request', async () => {
  const a = deferred(), b = deferred(), states = [];
  const loader = createLatestRequestLoader(value => states.push(value));
  let calls = 0;
  const api = () => ++calls === 1 ? a.promise : b.promise;
  const pa = loader.load(api, {modelFilter:'model-a'});
  const pb = loader.load(api, {modelFilter:'model-b'});
  a.reject(new Error('old request failed'));
  await pa;
  assert.equal(states.at(-1).loading, true);
  assert.equal(states.at(-1).error, null);
  b.resolve({data:[],summary:summary(200)});
  await pb;
  assert.equal(states.at(-1).loading, false);
});

test('cancelled requests cannot publish after unmount', async () => {
  const response = deferred(), states = [];
  const loader = createLatestRequestLoader(value => states.push(value));
  const pending = loader.load(() => response.promise, {});
  loader.cancel();
  response.resolve({data:[{}],summary:summary()});
  await pending;
  assert.equal(states.length, 1);
});

test('query failure clears the previous aggregate instead of retaining unrelated numbers', async () => {
  const states = [];
  const loader = createLatestRequestLoader(value => states.push(value));
  await loader.load(async () => ({data:[{}],summary:summary()}), {});
  await loader.load(async () => { throw new Error('unavailable'); }, {});
  assert.equal(states.at(-1).summary, null);
  assert.deepEqual(states.at(-1).logs, []);
  assert.equal(states.at(-1).error, 'unavailable');
  assert.equal(statisticsFromSummary(states.at(-1).summary), null);
});

test('missing summary is unavailable, but an empty summary is a valid zero window', async () => {
  let result;
  const loader = createLatestRequestLoader(value => {result=value;});
  await loader.load(async () => ({data:[{input_tokens:999}]}), {});
  assert.equal(result.summary, null);
  assert.ok(result.error);
  await loader.load(async () => ({data:[],summary:{requests:0,input_tokens:0,ttfb_samples:0}}), {});
  assert.equal(result.error, null);
  assert.equal(statisticsFromSummary(result.summary).requests, 0);
  assert.equal(statisticsFromSummary(result.summary).cacheRate, 0);
  for (const malformed of [{}, {requests:null}, {requests:-1}, {requests:'bad'}, {requests:0.5}]) {
    assert.equal(statisticsFromSummary(malformed),null);
  }
});

test('window cache and TTFB are weighted by their correct denominators', () => {
  const stats = statisticsFromSummary({...summary(),requests:10,input_tokens:10000,cache_read_tokens:8000,
    ttfb_millis_sum:12000,ttfb_samples:4,latency_millis_sum:30000});
  assert.equal(stats.cacheRate, 80);
  assert.equal(stats.avgTtfb, 3000);
  assert.equal(stats.avgLatency, 3000);
});

test('unknown credit is not presented as confirmed zero cost or included in known consumption', () => {
  const body={...summary(),requests:5,credits_consumed:99,credits_upstream:2,credits_estimated:3,
    credit_unknown_requests:2,credit_upstream_requests:2,credit_estimated_requests:1};
  assert.equal(statisticsFromSummary(body).credits, 5);
  assert.deepEqual(creditCoverage(body), {unknown:2,requests:5,percent:60,upstream:2,estimated:3});
  assert.equal(creditCoverage(summary()), null);
});

test('all query filters survive a refresh snapshot', () => {
  const q=buildRequestQuery({windowKey:'7d',modelFilter:'m',routeFilter:'/v1/responses',accountFilter:'a',
    regionFilter:'cn',statusFilter:'failed',ttfbMin:100.4,ttfbMax:500.6,idSearch:' req '},{now:1000000000});
  const params=new URLSearchParams(q.query);
  assert.equal(params.get('since'),String(1000000-604800));
  for (const [key,value] of [['model','m'],['route','/v1/responses'],['account','a'],['region','cn'],
    ['success','0'],['ttfb_min','100'],['ttfb_max','501'],['id','req']]) assert.equal(params.get(key),value);
  assert.equal(q.filters.ttfbMin,100);
  assert.equal(q.filters.ttfbMax,501);
});

// Exercise the component's actual refresh and metric selection, not a copy
// of the algorithm, without a browser or real model endpoints.
async function dashboardScenario(requestList) {
  const source=fs.readFileSync(new URL('../main.jsx',import.meta.url),'utf8').replace(/\r\n/g,'\n');
  const start=source.indexOf('  const refresh = async');
  const end=source.indexOf('\n  useEffect(() => {',start);
  assert.ok(start>=0 && end>start);
  const state={metricSamples:[{cacheScope:'1h',cacheRate:99,avgTTFB:9999}],dashboardWindow:'24h'};
  const callbacks=Object.fromEntries(['Data','Models','RequestLogs','DashboardSummary','DashboardScope','DashboardError',
    'DashboardUntil','DashboardLoading','Locked'].map(key=>['set'+key,value=>{state[key[0].toLowerCase()+key.slice(1)]=value;}]));
  const globals={...callbacks,AbortController,locked:false,dashboardWindow:'24h',
    refreshSerial:{current:0},refreshController:{current:null},
    buildRequestQuery,hasRequestSummary,statisticsFromSummary,
    setMetricSamples:fn=>{state.metricSamples=fn(state.metricSamples);},setSelectedModel:()=>{},setConfig:()=>{},
    api:async url=>{
      if (url==='/status') return {metrics:{requests:1000,input_tokens:10000,cache_read_tokens:9000,avg_ttfb_ms:9000},accounts:[]};
      if (url==='/v1/models') return {data:[]};
      if (url==='/admin/config') return {};
      if (url.startsWith('/requests?')) {
        if (requestList instanceof Error) throw requestList;
        return requestList;
      }
      throw new Error('unexpected mock endpoint');
    },
  };
  const ctx=vm.createContext(globals);
  new vm.Script(source.slice(start,end)+'\nglobalThis.runRefresh=refresh;').runInContext(ctx);
  await ctx.runRefresh();
  const metricsStart=source.indexOf('  const summaryForWindow =');
  const metricsEnd=source.indexOf('  const activeTab =',metricsStart);
  const view=vm.createContext({...state,statisticsFromSummary,fmtCredits:value=>Number(value||0).toFixed(4)});
  new vm.Script(source.slice(metricsStart,metricsEnd)+'\nglobalThis.result={windowStats,windowSamples,statCards};').runInContext(view);
  return {state,result:view.result};
}

test('dashboard request failure does not display the valid cumulative metrics', async () => {
  const {state,result}=await dashboardScenario(new Error('summary unavailable'));
  assert.equal(result.windowStats,null);
  assert.equal(result.statCards[0][1],undefined);
  assert.equal(state.dashboardError,'summary unavailable');
  assert.equal(state.dashboardLoading,false);
  assert.equal(state.metricSamples.length,1);
});

test('dashboard TTFB and samples use only the selected window summary', async () => {
  const {state,result}=await dashboardScenario({data:[],summary:{...summary(),requests:10,
    ttfb_millis_sum:12000,ttfb_samples:4}});
  assert.equal(result.windowStats.avgTtfb,3000);
  assert.equal(state.metricSamples.at(-1).avgTTFB,3000);
  assert.equal(result.windowSamples.length,1);
  assert.equal(result.windowSamples[0].cacheScope,'24h');
  assert.equal(state.dashboardError,null);
});
