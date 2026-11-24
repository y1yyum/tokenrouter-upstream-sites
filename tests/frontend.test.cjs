const assert = require('node:assert/strict')
const { readFileSync } = require('node:fs')
const { join } = require('node:path')
const { test } = require('node:test')
const { runInNewContext } = require('node:vm')

const source = readFileSync(join(__dirname, '..', 'frontend', 'index.js'), 'utf8')
const asOf = '2026-11-24T20:00:00Z'
const usd = { unit: 'USD', available: '100', multiplier: '2', source: 'configured', balance_cny: '50' }
const cny = { unit: 'CNY', available: '0', multiplier: '1', source: 'default', balance_cny: '0' }
const tokens = { unit: 'TOKENS', available: '0', multiplier: null, source: 'unset', balance_cny: null }
const failedWallet = { source: 'balance', outcome: 'authentication', at: asOf }

function siteFixture(overrides = {}) {
  return {
    id: 1, config_version: 7, wallet_epoch: 2, statistics_epoch: 3, provider_count: 2, has_key: true,
    config: {
      name: '测试站', base_url: 'https://example.test', timezone: 'Asia/Shanghai', adapter: 'sub2api',
      interval_minutes: 60, balance_multipliers: { USD: '2' }, endpoints: [], probes: [],
      policy: { mode: 'off' }, alerts: {},
    },
    latest: { source: 'balance', outcome: 'healthy', at: asOf, wallet_epoch: 2, balances: [usd] },
    last_observation: { source: 'probe', outcome: 'healthy', at: asOf },
    last_balance_observation: failedWallet,
    ...overrides,
  }
}

function reportFixture(overrides = {}) {
  return {
    as_of: asOf,
    current: {
      by_unit: [{ unit: 'USD', amount: '100', sites: 1 }, { unit: 'CNY', amount: '0', sites: 1 }],
      estimated_balance_cny: '50', fresh_sites: 1, priced_sites: 1, unpriced_sites: 1,
      stale_sites: 1, unknown_sites: 2, archived_sites: 3,
    },
    sites: [{ site_id: 1, status: 'fresh', balances: [usd], sampled_at: asOf }],
    periods: [],
    ...overrides,
  }
}

async function mount({ site = siteFixture(), report = reportFixture(), days = [], history = { items: [], total: 0 } } = {}) {
  const calls = []
  let mounted, render
  runInNewContext(source, {
    __TR_VUE__: {
      h(type, props, children) {
        if (arguments.length === 2 && (Array.isArray(props) || typeof props !== 'object' || props?.type)) {
          return { type, props: {}, children: props }
        }
        return { type, props: props || {}, children: typeof children === 'function' ? children() : children }
      },
      ref: value => ({ value }), computed: get => ({ get value() { return get() } }),
      onMounted: hook => { mounted = hook },
    },
    __TR_PLUGIN_SDK__: {
      TrCard: 'card', TrButton: 'button', TrBadge: 'badge',
      definePlugin: plugin => { render = plugin.pages.sites.setup() },
      useHost: () => ({ async request(method, path, options) {
        const call = JSON.parse(JSON.stringify({ method, path, ...options }))
        calls.push(call)
        if (typeof report === 'function' && path === '/statistics/overview') return { data: report(call) }
        const responses = {
          '/sites': { sites: [site] },
          '/control': { available: false, status: { events: [] } },
          '/statistics/overview': report, '/sites/1/daily': days, '/sites/1/alerts': [], '/sites/1/history': history,
        }
        return { data: responses[path] }
      } }),
    },
  })
  await mounted()
  return { render, calls }
}

function nodes(tree) {
  if (Array.isArray(tree)) return tree.flatMap(nodes)
  if (tree == null || typeof tree === 'boolean') return []
  return typeof tree === 'object' ? [tree, ...nodes(tree.children)] : [tree]
}

function visibleText(tree) {
  return nodes(tree).filter(node => typeof node !== 'object').join('\n')
}

function find(tree, type, label) {
  const node = nodes(tree).find(item => item.type === type && visibleText(item).includes(label))
  assert.ok(node, `${type} ${label}`)
  return node
}

function control(tree, label, type = 'input') {
  return find(find(tree, 'label', label), type, '')
}

function enter(node, value) {
  return (node.props.onInput || node.props.onChange)({ target: { value } })
}

test('倍率精确预览、非法输入拦截和清空完整映射', async () => {
  const { render, calls } = await mount()
  await find(render(), 'button', '编辑').props.onClick()
  assert.match(visibleText(render()), /20 USD ≈ 10 元/)
  assert.doesNotMatch(visibleText(render()), /首次查询前可选单位/)
  const multiplier = () => control(render(), 'USD（站内额度')
  enter(multiplier(), '0.0000000003')
  assert.match(visibleText(render()), /20 USD ≈ 66666666666\.6666666667 元/)
  for (const value of ['0', '-2', '2e1', '0.00000000001', '100000000000000000000']) {
    enter(multiplier(), value)
    assert.equal(multiplier().props['aria-invalid'], 'true')
  }
  await find(render(), 'button', '保存').props.onClick()
  assert.equal(calls.some(call => call.method === 'PUT'), false)
  enter(multiplier(), '')
  await find(render(), 'button', '保存').props.onClick()
  const saved = calls.find(call => call.method === 'PUT')
  assert.deepEqual(saved.body.config.balance_multipliers, {})
  assert.equal(saved.body.config_version, 7)
  assert.equal(saved.body.secrets.key, '')

  await find(render(), 'button', '新增站点').props.onClick()
  enter(control(render(), '首次查询前可选单位', 'select'), 'TOKENS')
  enter(control(render(), 'TOKENS（站内额度'), '0.5')
  assert.match(visibleText(render()), /20 TOKENS ≈ 40 元/)
  await find(render(), 'button', '保存').props.onClick()
  assert.deepEqual(calls.find(call => call.method === 'POST').body.config.balance_multipliers, { TOKENS: '0.5' })
})

test('时区只筛期间、钱包失败状态保留、部分计价与零额可见', async () => {
  const sourceSite = siteFixture({ latest: { at: asOf, balances: [usd, cny, tokens] } })
  const group = { timezone: 'Asia/Shanghai', from: '2026-11-25', to: '2026-11-25', estimated_cost_cny: '0', pricing_status: 'priced' }
  const { render, calls } = await mount({ site: sourceSite, report: call => reportFixture({
    sites: [{ site_id: 1, status: 'stale', balances: [usd, cny, tokens], period: group }],
    periods: call.query.timezone === 'UTC' ? [] : [group, { ...group, timezone: 'UTC' }],
  }) })
  assert.deepEqual(calls.find(call => call.path === '/statistics/overview').query, { period: 'today' })
  const first = visibleText(render())
  for (const label of ['50 CNY', '折合 0 CNY', '已过期', '已归档 3 站', '未知 2 站', '待设倍率', '部分计价', '凭据或访问被拒绝']) {
    assert.ok(first.includes(label), label)
  }
  await find(render(), 'button', '编辑').props.onClick()
  assert.match(visibleText(render()), /CNY（站内额度/)
  assert.match(visibleText(render()), /TOKENS（站内额度/)
  assert.match(visibleText(render()), /默认 1/)
  await find(render(), 'button', '取消').props.onClick()
  await enter(control(render(), '成本期间', 'select'), '7d')
  await enter(control(render(), '统计日历', 'select'), 'UTC')
  assert.deepEqual(calls.at(-1).query, { period: '7d', timezone: 'UTC' })
  assert.match(visibleText(render()), /50 CNY/)
  assert.match(visibleText(render()), /所选期间和统计日历暂无成本汇总/)
  assert.ok(calls.every(call => call.method === 'GET'))
})

test('记录范围按站点日历切换，混合旧日汇总保留部分计价', async () => {
  const current = { timezone: 'Asia/Shanghai', wallet_epoch: 2, statistics_epoch: 3, unit: 'USD' }
  const legacy = { ...current, algorithm_version: 1, date: '2026-11-24', estimated_spend: '5', net_change: '-5' }
  const priced = { ...current, algorithm_version: 2, date: '2026-11-25', pricing_status: 'priced',
    estimated_cost_cny: '0', gap_cost_cny: '0', upstream_cost: '0', comparable_intervals: 1, priced_intervals: 1,
    open_balance: '0', close_balance: '0', min_balance: '0', max_balance: '0', last_sample_at: asOf,
  }
  const old = { ...legacy, statistics_epoch: 2, date: '2026-11-20' }
  const partial = { ...priced, date: '2026-11-10', pricing_status: 'partial',
    estimated_cost_cny: '2', gap_cost_cny: '2', gap: true, unpriced_drop: '3',
    unpriced_reasons: { price_changed: 1 }, comparable_intervals: 2,
  }
  const { render, calls } = await mount({ days: [priced, legacy, partial, old] })
  await find(render(), 'button', '记录').props.onClick()
  const today = visibleText(render())
  assert.match(today, /2026-11-25 至 2026-11-25/)
  assert.match(today, /首笔 0 \/ 末笔 0/)
  assert.match(today, /最低 0 \/ 最高 0/)
  assert.match(today, /最后成功/)
  assert.match(today, /0 CNY/)
  assert.doesNotMatch(today, /部分计价 · 已计价小计/)
  const requestCount = calls.length
  enter(control(render(), '成本期间', 'select'), '7d')
  let summary = visibleText(find(render(), 'table', '观察到的下降'))
  assert.match(summary, /部分计价 · 已计价小计/)
  assert.match(summary, /旧日汇总缺少计价证据/)
  assert.doesNotMatch(summary, /跨倍率版本/)
  enter(control(render(), '成本期间', 'select'), '30d')
  summary = visibleText(find(render(), 'table', '观察到的下降'))
  assert.match(summary, /其中缺测补记 2 CNY/)
  assert.match(summary, /跨倍率版本 1 段/)
  assert.match(visibleText(render()), /当日无实测余额/)
  enter(control(render(), '记录分段', 'select'), JSON.stringify([2, 2, 'Asia/Shanghai']))
  summary = visibleText(find(render(), 'table', '观察到的下降'))
  assert.match(summary, /成本未知/)
  assert.doesNotMatch(summary, /部分计价|0 CNY/)
  assert.match(visibleText(render()), /2026-11-20 至 2026-11-20/)
  assert.match(visibleText(render()), /余额下降估算 5/)
  assert.equal(calls.length, requestCount)
})
