/* index.js mounts a standalone administrator page through the official plugin SDK.
 * All API calls stay within this plugin's namespace. Credentials are write-only,
 * binding changes require an explicit save, and provider controls default to disabled.
 */
(function () {
  const { h, ref, computed, onMounted } = __TR_VUE__
  const { definePlugin, useHost, TrButton, TrCard, TrBadge } = __TR_PLUGIN_SDK__
  const statuses = { healthy: '正常', authentication: '凭据或访问被拒绝', unknown: '未取得站点余额', unreachable: '上游不可访问', rate_limited: '查询被限流', monitor_failure: '监控请求失败', probe_unhealthy: '探针业务异常', configuration: '配置错误' }
  const protocols = ['openai_chat_completions', 'openai_responses', 'anthropic_messages', 'gemini_generate_content', 'custom']
  const adapters = { sub2api: 'Sub2API', new_api: 'New API', deepseek_balance: 'DeepSeek', kimi_balance: 'Kimi', zivv: 'Zivv', derouter: 'DeRouter' }
  const units = ['USD', 'CNY', 'TOKENS']
  const adapterWalletUnits = { sub2api: 'USD', derouter: 'USD', kimi_balance: 'CNY' }
  const periods = [['today', '今日'], ['7d', '近 7 日'], ['30d', '近 30 日'], ['retained', '保留期']]
  const balanceStatuses = { fresh: '有效样本', stale: '已过期', unknown: '未知', archived: '已归档' }
  const pricingStatuses = { priced: '已计价', partial: '部分计价', unknown: '成本未知' }
  const unpricedReasons = {
    missing_price_snapshot: '缺少历史倍率快照',
    missing_multiplier: '未设倍率',
    invalid_multiplier: '历史倍率无效',
    price_changed: '跨倍率版本',
  }
  const amountScale = 10n ** 10n
  const amountPattern = /^([+-]?)(\d*)(?:\.(\d{0,10}))?$/

  /** normalizeURL groups exact roots while preserving deployment paths and domains. */
  function normalizeURL(value) { try { const u = new URL(value); return u.origin.toLowerCase() + u.pathname.replace(/\/+$/, '') } catch { return '' } }
  /** displayTime renders saved timestamps in the browser's selected local timezone. */
  function displayTime(value) { return value ? new Date(value).toLocaleString() : '尚未查询' }

  function amountText(value, unit) {
    return value == null || value === '' ? '未知' : `${value}${unit ? ` ${unit}` : ''}`
  }

  function fixedAmount(value) {
    const text = String(value ?? '')
    const match = text.match(amountPattern)
    if (!match || match[0] !== text || (!match[2] && !match[3])) return null
    const [, sign, whole, fraction = ''] = match
    const magnitude = BigInt(whole + fraction.padEnd(10, '0'))
    return sign === '-' ? -magnitude : magnitude
  }

  function decimalText(value) {
    const magnitude = (value < 0n ? -value : value).toString().padStart(11, '0')
    const fraction = magnitude.slice(-10).replace(/0+$/, '')
    return `${value < 0n ? '-' : ''}${magnitude.slice(0, -10)}${fraction ? `.${fraction}` : ''}`
  }

  function multiplierError(value) {
    if (value === '') return ''
    const amount = value.length <= 64 ? fixedAmount(value) : null
    return amount == null || amount <= 0n || amount >= 10n ** 20n * amountScale
      ? '请输入大于 0 且小于 10²⁰ 的十进制数，最多 10 位小数。'
      : ''
  }

  function divideAmount(value, multiplier) {
    const amount = fixedAmount(value), divisor = fixedAmount(multiplier)
    if (amount == null || divisor == null || divisor <= 0n) return null
    const magnitude = (amount < 0n ? -amount : amount) * amountScale
    const rounded = magnitude / divisor + (magnitude % divisor * 2n >= divisor ? 1n : 0n)
    return decimalText(amount < 0n ? -rounded : rounded)
  }

  function currentBalances(site) {
    if (site.latest?.wallet_epoch != null && site.latest.wallet_epoch !== site.wallet_epoch) return []
    return (site.latest?.balances || []).map(balance => {
      const configured = site.config.balance_multipliers?.[balance.unit]
      const multiplier = configured ?? (balance.unit === 'CNY' ? '1' : null)
      return {
        ...balance,
        multiplier,
        source: configured != null ? 'configured' : multiplier == null ? 'unset' : 'default',
        balance_cny: divideAmount(balance.available, multiplier),
      }
    })
  }

  function calendarDay(at, timezone) {
    try {
      const parts = new Intl.DateTimeFormat('en-CA', {
        timeZone: timezone === 'Local' ? undefined : timezone,
        year: 'numeric', month: '2-digit', day: '2-digit',
      }).formatToParts(new Date(at))
      const date = Object.fromEntries(parts.map(part => [part.type, part.value]))
      return `${date.year}-${date.month}-${date.day}`
    } catch { return '' }
  }

  function periodRange(period, timezone, dates, asOf) {
    const to = calendarDay(asOf, timezone)
    if (period === 'retained') {
      const retained = dates.filter(date => !to || date <= to)
      return { from: retained[0], to: retained.at(-1) }
    }
    if (!to) return {}
    const daysBack = { today: 0, '7d': 6, '30d': 29 }[period]
    const day = new Date(`${to}T00:00:00Z`)
    day.setUTCDate(day.getUTCDate() - daysBack)
    return { from: day.toISOString().slice(0, 10), to }
  }

  function sumAmount(a, b) {
    const first = fixedAmount(a), second = fixedAmount(b)
    return first == null && second == null ? null : decimalText((first ?? 0n) + (second ?? 0n))
  }

  function summarizeDays(rows) {
    const byUnit = new Map()
    for (const row of rows) {
      const total = byUnit.get(row.unit) || { unit: row.unit, covered_seconds: 0, priced_intervals: 0, comparable_intervals: 0, unpriced_reasons: {} }
      for (const key of ['upstream_cost', 'estimated_cost_cny', 'gap_cost_cny', 'unpriced_drop']) {
        total[key] = sumAmount(total[key], row[key])
      }
      for (const key of ['covered_seconds', 'priced_intervals', 'comparable_intervals']) {
        total[key] += row[key] ?? 0
      }
      for (const [reason, count] of Object.entries(row.unpriced_reasons || {})) {
        total.unpriced_reasons[reason] = (total.unpriced_reasons[reason] ?? 0) + count
      }
      total.gap ||= row.gap
      total.has_unknown_coverage ||= hasUnknownCoverage(row)
      total.hasPartialDay ||= row.pricing_status === 'partial'
      total.pricing_status = total.priced_intervals === 0 ? 'unknown'
        : total.hasPartialDay || total.has_unknown_coverage || total.priced_intervals < total.comparable_intervals ? 'partial' : 'priced'
      byUnit.set(row.unit, total)
    }
    return [...byUnit.values()]
  }

  function hasUnknownCoverage(row) {
    if (row.has_unknown_coverage === true) return true
    return row.algorithm_version === 1 && (row.pricing_status === 'partial' ||
      (row.estimated_cost_cny == null && !row.comparable_intervals && (row.estimated_spend != null || row.net_change != null)))
  }

  definePlugin({ pages: { sites: {
    setup() {
      const host = useHost()
      const sites = ref([]), loading = ref(false), message = ref(''), error = ref(''), search = ref('')
      const control = ref({ available: false, status: { holds: [], events: [] } })
      const tab = ref('sites'), selected = ref(null), form = ref(null), secrets = ref({}), providers = ref([]), bindings = ref({}), bindingVersion = ref(0), bindingDirty = ref(false)
      const history = ref([]), historyTotal = ref(0), offset = ref(0), daily = ref([]), alerts = ref([])
      const report = ref(null), reportError = ref(''), period = ref('today'), timezone = ref('')
      const detailPeriod = ref('today'), detailSegment = ref(''), firstUnit = ref('')
      const siteReports = computed(() => new Map((report.value?.sites || []).map(site => [site.site_id, site])))
      const timezones = computed(() => [...new Set([
        ...sites.value.filter(site => !site.archived).map(site => site.config.timezone || 'UTC'),
        ...(report.value?.periods || []).map(group => group.timezone),
        timezone.value,
      ].filter(Boolean))].sort())
      const filtered = computed(() => sites.value.filter(s => (s.config.name + ' ' + s.config.base_url).toLowerCase().includes(search.value.toLowerCase())))
      /** api unwraps only this plugin's response envelope and leaves host auth to SDK. */
      async function api(method, path, body, query) { const r = await host.request(method, path, { body, query }); return r.data }
      /** perform contains action errors and prevents duplicate form submissions. */
      async function perform(work) { if (loading.value) return; loading.value = true; error.value = ''; message.value = ''; try { await work() } catch (e) { error.value = e.message || '操作失败' } finally { loading.value = false } }
      async function refreshReport() {
        const query = { period: period.value }
        if (timezone.value) query.timezone = timezone.value
        try {
          report.value = await api('GET', '/statistics/overview', undefined, query)
          reportError.value = ''
        } catch {
          report.value = null
          reportError.value = '暂时无法读取余额与成本总览。可用「刷新列表」重试。'
        }
      }
      /** refresh retrieves public site metadata without requesting credential values. */
      async function refresh() {
        const [result] = await Promise.all([
          api('GET', '/sites'),
          api('GET', '/control').then(value => { control.value = value }).catch(() => {
            control.value = { available: false, status: { holds: [], events: [] } }
          }),
          refreshReport(),
        ])
        sites.value = result.sites || []
        if (selected.value) selected.value = sites.value.find(site => site.id === selected.value.id) || null
      }
      /** button uses the host's native themed button instead of copying its palette. */
      function button(label, action, secondary = true, disabled = false) { return h(TrButton, { variant: secondary ? 'secondary' : 'primary', disabled: loading.value || disabled, onClick: () => perform(action) }, () => label) }
      /** input binds plain text without interpreting saved strings as markup. */
      function input(value, change, attrs = {}) { return h('input', { ...attrs, value, onInput: e => change(e.target.value) }) }
      /** field associates a visible description with its input control. */
      function field(label, value, change, attrs = {}) { return h('label', { class: 'us-field' }, [h('span', label), input(value, change, attrs)]) }
      /** select renders a bounded choice list using native keyboard interaction. */
      function select(value, choices, change, attrs = {}) { return h('select', { ...attrs, value, onChange: e => change(e.target.value) }, choices.map(([v, label]) => h('option', { value: v }, label))) }
      /** toggle preserves explicit boolean state rather than coercing text inputs. */
      function toggle(label, value, change) { return h('label', { class: 'us-check' }, [h('input', { type: 'checkbox', checked: value, onChange: e => change(e.target.checked) }), label]) }
      /** table renders empty and populated data with the same accessible headings. */
      function table(headings, rows) { return h('div', { class: 'us-scroll' }, [h('table', [h('thead', h('tr', headings.map(v => h('th', { scope: 'col' }, v)))), h('tbody', rows.length ? rows.map(row => h('tr', row.map(v => h('td', v)))) : h('tr', h('td', { colspan: headings.length }, '暂无记录')))])]) }
      /** openEditor copies configuration so canceling leaves the saved site unchanged. */
      function openEditor(site) {
        form.value = site ? JSON.parse(JSON.stringify(site)) : { config_version: 0, archived: false, config: { name: '', notes: '', base_url: '', adapter: 'sub2api', endpoints: [], auto_query: false, interval_minutes: 60, timezone: 'Asia/Shanghai', raw_retention_days: 90, summary_retention_days: 365, policy: { mode: 'off', auto_recover: false, thresholds: [], balance_match: 'any', low_count: 1, failure_count: 3, recovery_count: 2, min_suspend_seconds: 120 }, probes: [], alerts: { daily_metric: 'estimated', control: false, unit: 'USD', low_balance: '', daily_spend: '', failures: 0, email: '' } } }
        form.value.config.balance_multipliers ||= {}
        firstUnit.value = ''
        secrets.value = { key: '', wallet_token: '', wallet_user_id: '', proxy_url: '' }; tab.value = 'edit'
      }
      function multiplierFields(site) {
        const configured = site.config.balance_multipliers
        const observed = new Set((site.latest?.balances || []).map(balance => balance.unit))
        const selectedUnit = firstUnit.value || (!observed.size && adapterWalletUnits[site.config.adapter]) || ''
        const shown = units.filter(unit => observed.has(unit) || Object.hasOwn(configured, unit) || selectedUnit === unit)
        return h('fieldset', [
          h('legend', '余额倍率（选填）'),
          h('p', { class: 'us-muted' }, '填写实付 1 元可得的站内额度。CNY 留空按默认 1 换算，其他单位留空显示待设倍率。'),
          !observed.size ? h('label', { class: 'us-field us-filter' }, [
            '首次查询前可选单位',
            select(selectedUnit, [['', '请选择单位'], ...units.map(unit => [unit, unit])], value => { firstUnit.value = value }),
          ]) : null,
          h('div', { class: 'us-grid' }, shown.map(unit => {
            const value = configured[unit] ?? ''
            const problem = multiplierError(value)
            const rate = value || (unit === 'CNY' ? '1' : '')
            const preview = rate && !problem
              ? `${value ? '' : '默认 1。'}1 元 = ${decimalText(fixedAmount(rate))} ${unit}，20 ${unit} ≈ ${divideAmount('20', rate)} 元`
              : '待设倍率'
            const hint = `us-multiplier-${unit}`
            return h('div', { key: unit }, [
              field(`${unit}（站内额度 / 人民币元）`, value, next => { configured[unit] = next }, {
                inputmode: 'decimal', placeholder: unit === 'CNY' ? '默认 1' : '例如 2',
                'aria-invalid': problem ? 'true' : undefined, 'aria-describedby': hint,
              }),
              h('small', { id: hint, class: problem ? 'us-field-error' : 'us-muted' }, problem || preview),
            ])
          })),
        ])
      }
      /** save persists the complete reviewed form while keeping automatic recovery off when control itself is disabled. */
      async function save() {
        const f = form.value
        const multipliers = Object.entries(f.config.balance_multipliers).filter(([, value]) => value !== '')
        const invalid = multipliers.find(([, value]) => multiplierError(value))
        if (invalid) throw new Error(`${invalid[0]} 余额倍率：${multiplierError(invalid[1])}`)
        const config = { ...f.config, balance_multipliers: Object.fromEntries(multipliers) }
        if (config.policy.mode === 'off') config.policy.auto_recover = false
        await api(f.id ? 'PUT' : 'POST', f.id ? `/sites/${f.id}` : '/sites', { config, config_version: f.config_version, archived: f.archived, secrets: secrets.value })
        secrets.value = {}; form.value = null; tab.value = 'sites'; await refresh(); message.value = '站点已保存'
      }
      /** query fetches one wallet without enabling automatic provider control. */
      async function query(site, source = 'balance') { const o = await api('POST', `/sites/${site.id}/query`, undefined, { source }); await refresh(); message.value = `${site.config.name}：${statuses[o.outcome] || o.outcome}` }
      /** queryAll caps concurrent manual wallet requests to four independent sites. */
      async function queryAll() { const queue = sites.value.filter(s => !s.archived).slice(); let success = 0, failed = 0; await Promise.all(Array.from({ length: 4 }, async () => { while (queue.length) { const s = queue.shift(); try { const o = await api('POST', `/sites/${s.id}/query`); if (o.outcome === 'healthy') success++; else failed++ } catch { failed++ } } })); await refresh(); message.value = `查询完成：${success} 个正常，${failed} 个未取得有效余额` }
      async function loadHistory(site, start) {
        const result = await api('GET', `/sites/${site.id}/history`, undefined, { offset: String(start) })
        history.value = result.items || []
        historyTotal.value = result.total
        offset.value = start
      }
      /** loadDetails requests paginated observations plus independently retained summaries. */
      async function loadDetails(site, start = 0) {
        const [days, events] = await Promise.all([
          api('GET', `/sites/${site.id}/daily`),
          api('GET', `/sites/${site.id}/alerts`),
          loadHistory(site, start),
        ])
        if (start === 0 && tab.value !== 'history') {
          detailSegment.value = ''
          detailPeriod.value = period.value
        }
        selected.value = site
        daily.value = days || []
        alerts.value = events || []
        tab.value = 'history'
      }
      /** loadBindings reads live providers and the plugin's optimistic mapping version. */
      async function loadBindings() { const [p, b] = await Promise.all([api('GET', '/providers'), api('GET', '/bindings')]); providers.value = p; bindings.value = b.bindings; bindingVersion.value = b.version; bindingDirty.value = false; tab.value = 'bindings' }
      /** suggestBindings fills unique URL matches in the unsaved review form only. */
      async function suggestBindings() { let matches = 0; for (const p of providers.value) { if (bindings.value[p.id]) continue; const key = normalizeURL(p.base_url); if (!key) continue; const candidates = sites.value.filter(s => !s.archived && [s.config.base_url, ...s.config.endpoints.map(e => e.url)].some(u => normalizeURL(u) === key)); if (candidates.length === 1) { bindings.value[p.id] = candidates[0].id; matches++ } } bindingDirty.value = bindingDirty.value || matches > 0; message.value = `已建议 ${matches} 个绑定，请核对后保存；多站点匹配不自动选择` }
      /** saveBindings commits the reviewed mapping without modifying provider URLs or keys. */
      async function saveBindings() { const r = await api('PUT', '/bindings', { bindings: bindings.value, version: bindingVersion.value }); bindingVersion.value = r.version; bindingDirty.value = false; await refresh(); message.value = '绑定已保存，提供商转发配置保持不变' }
      /** endpointFields edits copyable protocol templates independently of provider URLs. */
      function endpointFields(c) { return h('fieldset', [h('legend', '协议地址'), ...c.endpoints.map((e, i) => h('div', { class: 'us-row' }, [field('名称', e.name, v => e.name = v), field('协议', e.protocol, v => e.protocol = v, { list: 'us-protocols' }), field('BaseURL', e.url, v => e.url = v, { type: 'url' }), button('移除', async () => c.endpoints.splice(i, 1))])), h('datalist', { id: 'us-protocols' }, protocols.map(p => h('option', { value: p }))), button('添加地址', async () => c.endpoints.push({ id: crypto.randomUUID(), name: '', protocol: 'openai_chat_completions', url: c.base_url, default: false }))]) }
      /** probeFields configures read-only health checks with explicit response expectations. */
      function probeFields(c) { return h('fieldset', [h('legend', '定时探针'), ...c.probes.map((p, i) => h('div', { class: 'us-probe' }, [h('div', { class: 'us-row' }, [field('探针名称', p.id, v => p.id = v), field('完整 URL', p.url, v => p.url = v, { type: 'url' }), field('间隔（分钟）', p.interval_minutes, v => p.interval_minutes = Number(v), { type: 'number', min: 1, max: 1440 })]), h('div', { class: 'us-row' }, [field('响应 JSON 字段', p.json_path, v => p.json_path = v, { placeholder: '例如 success' }), field('期望值', p.expected, v => p.expected = v), toggle('参与站点停用控制', p.required, v => p.required = v), toggle('使用余额 Key（必须同源）', p.use_wallet_key, v => p.use_wallet_key = v), button('移除', async () => c.probes.splice(i, 1))])])), button('添加探针', async () => c.probes.push({ id: `probe-${c.probes.length + 1}`, url: c.base_url, interval_minutes: 5, timeout_seconds: 10, json_path: 'success', expected: 'true', use_wallet_key: false, success_codes: [200], required: false }))]) }
      /** controlFields exposes independent stop/recovery thresholds and source selection. */
      function controlFields(c) {
        const p = c.policy
        return h('fieldset', [h('legend', '提供商自动停用与恢复'),
          !control.value.available ? h('p', { class: 'us-error' }, '控制保护连接未就绪，暂不能开启自动启停。余额查询仍可使用。') : null,
          h('label', { class: 'us-field' }, ['控制方式', select(p.mode, [['off', '关闭'], ['balance', '余额阈值 / 查询不可访问'], ['probe', '定时探针'], ['combined', '余额与探针共同控制']], v => { p.mode = v; if (v === 'off') p.auto_recover = false })]),
          h('p', { class: 'us-muted' }, '停用范围为本站点全部绑定提供商。401/403、限流、解析错误或监控代理故障不按零余额处理。关闭控制会解除插件停用，但保留人工或系统的停用选择。'),
          p.mode !== 'off' ? h('div', [toggle('符合恢复条件时自动恢复', p.auto_recover, v => p.auto_recover = v),
            h('div', { class: 'us-grid' }, [field('连续低余额次数', p.low_count, v => p.low_count = Number(v), { type: 'number', min: 1, max: 100 }), field('连续不可访问 / 探针异常次数', p.failure_count, v => p.failure_count = Number(v), { type: 'number', min: 1, max: 100 }), field('连续正常恢复次数', p.recovery_count, v => p.recovery_count = Number(v), { type: 'number', min: 1, max: 100 }), field('最短停用时间（秒）', p.min_suspend_seconds, v => p.min_suspend_seconds = Number(v), { type: 'number', min: 0, max: 86400 })]),
            ['balance', 'combined'].includes(p.mode) ? h('div', [h('label', { class: 'us-field' }, ['多币种判断', select(p.balance_match, [['any', '任一币种低余额则停用；全部恢复才恢复'], ['all', '全部币种低余额才停用；任一恢复则恢复']], v => p.balance_match = v)]),
              ...p.thresholds.map((t, i) => h('div', { class: 'us-row' }, [select(t.unit, [['USD', 'USD'], ['CNY', 'CNY'], ['TOKENS', 'TOKENS']], v => t.unit = v), field('低于此余额停用', t.low, v => t.low = v), field('达到此余额恢复', t.recover, v => t.recover = v), button('移除阈值', async () => p.thresholds.splice(i, 1))])), button('添加余额阈值', async () => p.thresholds.push({ unit: 'USD', low: '1', recover: '5' }))]) : null,
            ['probe', 'combined'].includes(p.mode) ? h('p', '请至少为一个定时探针勾选“参与站点停用控制”。多个原因必须全部解除，才会恢复提供商。') : null,
            h('p', { class: 'us-muted' }, '人工停用、原本停用及系统错误停用不会被自动恢复。停用期间人工开启的选择会被记住，但需等站点恢复后才生效。连续相同证据每 30 秒最多计数一次。')]) : null])
      }
      /** editor renders account-level configuration while treating all secrets as write-only. */
      function editor() { const f = form.value, c = f.config; return h(TrCard, null, () => [h('h2', f.id ? '编辑上游站点' : '新增上游站点'), h('div', { class: 'us-grid' }, [field('名称', c.name, v => c.name = v), field('余额查询 BaseURL', c.base_url, v => c.base_url = v, { type: 'url' }), h('label', { class: 'us-field' }, ['上游类型', select(c.adapter, Object.entries(adapters), v => c.adapter = v)]), field('余额查询 Key', secrets.value.key, v => secrets.value.key = v, { type: 'password', autocomplete: 'new-password', placeholder: f.has_key ? '已保存；留空保留' : '请输入可查询站点余额的 Key' }), field('查询间隔（分钟）', c.interval_minutes, v => c.interval_minutes = Number(v), { type: 'number', min: 5, max: 1440 }), field('统计时区', c.timezone, v => c.timezone = v)]), h('p', { class: 'us-muted' }, '请填入能查询站点账户余额的 Key；不使用单个 Key 的额度作为余额。密钥留空保留原值。'), c.adapter === 'new_api' ? h('div', { class: 'us-row' }, [field('账户访问令牌（可选）', secrets.value.wallet_token, v => secrets.value.wallet_token = v, { type: 'password', autocomplete: 'new-password' }), field('账户用户 ID（可选）', secrets.value.wallet_user_id, v => secrets.value.wallet_user_id = v), h('p', { class: 'us-muted' }, '如 API Key 只返回 Key 额度，请补充能读取账户余额的账户访问令牌；用户 ID 必须与该令牌所属账户一致。')]) : null, toggle('自动查询余额', c.auto_query, v => c.auto_query = v), multiplierFields(f), endpointFields(c), probeFields(c), controlFields(c), h('fieldset', [h('legend', '告警阈值'), h('div', { class: 'us-grid' }, [h('label', { class: 'us-field' }, ['币种', select(c.alerts.unit || 'USD', [['USD', 'USD'], ['CNY', 'CNY'], ['TOKENS', 'TOKENS']], v => c.alerts.unit = v)]), field('低余额（留空关闭）', c.alerts.low_balance, v => c.alerts.low_balance = v), field('每日消耗（留空关闭）', c.alerts.daily_spend, v => c.alerts.daily_spend = v), field('连续失败次数（0 关闭）', c.alerts.failures, v => c.alerts.failures = Number(v), { type: 'number', min: 0, max: 100 }), h('label', { class: 'us-field' }, ['消耗口径', select(c.alerts.daily_metric, [['estimated', '余额下降估算'], ['account', '账户累计消费差额']], v => c.alerts.daily_metric = v)])]), h('p', { class: 'us-muted' }, '告警保存在本站点记录中。邮件接收人在插件设置的“告警接收用户 ID”中指定；0 表示不发邮件。每日消耗只告警。')]), field('查询代理（可选，留空保留）', secrets.value.proxy_url, v => secrets.value.proxy_url = v, { type: 'password', autocomplete: 'new-password', placeholder: f.has_proxy ? '已设置代理' : 'http:// 或 https://' }), h('label', { class: 'us-field' }, ['备注', h('textarea', { value: c.notes, rows: 3, onInput: e => c.notes = e.target.value })]), toggle('归档（需先解绑，归档后停止查询）', f.archived, v => f.archived = v), h('div', { class: 'us-actions' }, [button('保存', save, false), button('取消', async () => { secrets.value = {}; form.value = null; tab.value = 'sites' })])]) }
      function costSummary(row) {
        return h('div', { class: 'us-stack' }, [
          h('strong', amountText(row.estimated_cost_cny, 'CNY')),
          h('small', row.pricing_status === 'partial' ? '部分计价 · 已计价小计' : pricingStatuses[row.pricing_status] || '成本未知'),
          h('small', `其中缺测补记 ${amountText(row.gap_cost_cny, 'CNY')}${row.gap ? ' · 有缺测' : ''}`),
        ])
      }
      function pricingCoverage(row) {
        const reasons = Object.entries(row.unpriced_reasons || {})
          .map(([reason, count]) => `${unpricedReasons[reason] || reason} ${count} 段`)
        return h('div', { class: 'us-stack' }, [
          h('span', row.comparable_intervals > 0 ? `已计价 ${row.priced_intervals ?? 0} / ${row.comparable_intervals} 段` : '—'),
          h('small', `未计价下降 ${amountText(row.unpriced_drop, row.unit)}`),
          reasons.length ? h('small', reasons.join('、')) : null,
          hasUnknownCoverage(row) ? h('small', '旧日汇总缺少计价证据，区间数未知') : null,
        ])
      }
      function calendarRange(range) {
        return range.from && range.to ? `${range.from} 至 ${range.to}` : '暂无日期范围'
      }
      function periodFilter(value, change, disabled = false) {
        return h('label', { class: 'us-field us-filter' }, ['成本期间', select(value, periods, change, { disabled })])
      }
      function currentOverview(current, asOf) {
        return [
          h('h2', '当前余额总览'),
          h('div', { class: 'us-metrics' }, [
            h('div', [h('span', '折合人民币小计'), h('strong', amountText(current.estimated_balance_cny, 'CNY')),
              h('small', `已折合 ${current.priced_sites} 站 · 含待设倍率 ${current.unpriced_sites} 站`)]),
            h('div', [h('span', '参与站点'), h('strong', String(current.fresh_sites)),
              h('small', `已过期 ${current.stale_sites} 站 · 未知 ${current.unknown_sites} 站 · 已归档 ${current.archived_sites} 站`)]),
          ]),
          table(['原单位', '当前可用余额', '参与站点'], (current.by_unit || []).map(row => [row.unit, amountText(row.amount, row.unit), String(row.sites)])),
          asOf ? h('small', `统计时间 ${displayTime(asOf)} · 仅计入有效样本`) : null,
        ]
      }
      function costOverview(groups) {
        return groups.length ? groups.map(group => h('section', { key: group.timezone, class: 'us-period' }, [
          h('h4', group.timezone),
          h('small', calendarRange(group)),
          h('div', { class: 'us-period-total' }, costSummary(group)),
          table(['原单位', '观察到的下降', '估算成本', '计价覆盖'], (group.by_unit || []).map(row => [
            row.unit, amountText(row.upstream_cost, row.unit), costSummary(row), pricingCoverage(row),
          ])),
        ])) : h('p', { class: 'us-muted' }, '所选期间和统计日历暂无成本汇总。')
      }
      function walletStatus(site, row) {
        const observation = row?.last_balance_observation || site.last_balance_observation ||
          (site.last_observation?.source === 'balance' ? site.last_observation : site.latest)
        return h('div', { class: 'us-stack' }, [
          h(TrBadge, { tone: observation?.outcome === 'healthy' ? 'success' : 'warning' }, () => statuses[observation?.outcome] || '尚未查询'),
          observation?.at ? h('small', displayTime(observation.at)) : null,
        ])
      }
      function siteBalances(site, row) {
        const balances = row?.balances ?? currentBalances(site)
        const hasMissing = balances.some(balance => balance.source === 'unset' || balance.multiplier == null || balance.multiplier === '')
        const hasPrice = balances.some(balance => balance.balance_cny != null)
        const sampledAt = row?.sampled_at ?? (balances.length ? site.latest?.at : null)
        const expired = Date.now() - Date.parse(sampledAt) > 2 * site.config.interval_minutes * 60000
        const status = site.archived ? 'archived' : row?.status || (!sampledAt ? 'unknown' : expired ? 'stale' : 'fresh')
        const partial = row?.balance_pricing_status ? row.balance_pricing_status === 'partial' : hasPrice && hasMissing
        return h('div', { class: 'us-stack us-balances' }, [
          ...balances.map(balance => h('div', { key: balance.unit, class: 'us-balance' }, [
            h('strong', amountText(balance.available, balance.unit)),
            h('small', balance.source === 'default' ? '倍率 默认 1' : balance.multiplier ? `倍率 ${balance.multiplier}` : '待设倍率'),
            h('span', `折合 ${amountText(balance.balance_cny, 'CNY')}`),
          ])),
          h('div', { class: 'us-actions' }, [
            h(TrBadge, { tone: status === 'stale' ? 'warning' : 'neutral' }, () => balanceStatuses[status] || status),
            partial ? h(TrBadge, { tone: 'warning' }, () => '部分计价') : null,
          ]),
          sampledAt ? h('small', `取样 ${displayTime(sampledAt)}`) : null,
        ])
      }
      function sitePeriod(group) {
        return group ? h('div', { class: 'us-stack' }, [
          h('small', group.timezone),
          h('small', calendarRange(group)),
          costSummary(group),
          ...(group.by_unit || []).map(row => h('small', { key: row.unit }, `下降 ${amountText(row.upstream_cost, row.unit)}`)),
        ]) : '—'
      }
      function overview() {
        return [
          h(TrCard, null, () => [
            reportError.value ? h('p', { role: 'status', class: 'us-muted' }, reportError.value) : null,
            ...(report.value?.current ? currentOverview(report.value.current, report.value.as_of) : []),
            h('h2', '期间估算成本'),
            h('div', { class: 'us-row' }, [
              periodFilter(period.value, value => perform(async () => {
                period.value = value
                await refreshReport()
              }), loading.value),
              h('label', { class: 'us-field us-filter' }, ['统计日历', select(timezone.value,
                [['', '按各站点时区分组'], ...timezones.value.map(zone => [zone, zone])], value => perform(async () => {
                  timezone.value = value
                  await refreshReport()
                }), { disabled: loading.value })]),
            ]),
            report.value ? costOverview(report.value.periods || []) : null,
          ]),
          h(TrCard, null, () => [
            h('h2', '站点列表'),
            h('div', { class: 'us-actions' }, [
              input(search.value, value => { search.value = value }, { placeholder: '搜索站点名称或地址', 'aria-label': '搜索站点' }),
              button('刷新列表', refresh), button('查询全部余额', queryAll), button('新增站点', async () => openEditor(null), false),
            ]),
            table(['站点 / 余额查询地址', '提供商', '最近成功余额', '最近已知余额查询', '期间估算成本', '自动查询 / 控制', '操作'], filtered.value.map(site => {
              const row = siteReports.value.get(site.id)
              return [
                h('div', [h('strong', site.config.name), h('div', { class: 'us-url' }, site.config.base_url)]),
                String(site.provider_count), siteBalances(site, row), walletStatus(site, row), sitePeriod(row?.period),
                h('div', [site.config.auto_query && !site.archived ? `每 ${site.config.interval_minutes} 分钟` : '查询关闭',
                  h('div', site.config.policy.mode === 'off' ? '控制关闭' : (Object.keys(site.state?.reasons || {}).length ? '站点已停用：' + Object.keys(site.state.reasons).join('、') : '监控中')),
                  h('small', site.config.policy.auto_recover ? '自动恢复开启' : '自动恢复关闭')]),
                h('div', { class: 'us-actions' }, [button('查询', () => query(site)), button('编辑', async () => openEditor(site)), button('记录', () => loadDetails(site))]),
              ]
            })),
          ]),
        ]
      }
      /** providerStatus distinguishes the plugin's gate from external scheduling intent. */
      function providerStatus(p) { const hold = (control.value.status?.holds || []).find(x => x.provider_id === p.id); return hold ? `${p.status} / 插件停用；人工或系统选择：${hold.manual_schedulable ? '启用' : '停用'}` : `${p.status} / ${p.schedulable ? '可调度' : '不可调度'}` }
      /** bindingPage allows custom URL providers to bind independently of their address. */
      function bindingPage() { return h(TrCard, null, () => [h('h2', '提供商绑定'), h('p', '绑定只用于站点归属。提供商可继续使用自定义 BaseURL；选择“未绑定 / 官方”可独立使用。'), h('div', { class: 'us-actions' }, [button('扫描并建议绑定', suggestBindings), button('保存绑定', saveBindings, false), bindingDirty.value ? h('strong', '有尚未保存的修改') : null]), table(['提供商', '当前 BaseURL', '手动状态', '上游站点'], providers.value.map(p => [p.name, h('span', { class: 'us-url' }, p.base_url), providerStatus(p), select(String(bindings.value[p.id] || ''), [['', '未绑定 / 官方'], ...sites.value.filter(s => !s.archived).map(s => [String(s.id), s.config.name])], v => { if (v) bindings.value[p.id] = Number(v); else delete bindings.value[p.id]; bindingDirty.value = true })]))]) }
      function dailySummary(site) {
        const current = { wallet_epoch: site.wallet_epoch, statistics_epoch: site.statistics_epoch, timezone: site.config.timezone || 'UTC' }
        const segmentKey = row => JSON.stringify([row.wallet_epoch, row.statistics_epoch, row.timezone || 'UTC'])
        const currentKey = segmentKey(current)
        const segments = new Map(daily.value.map(row => [segmentKey(row), row]))
        segments.delete(currentKey)
        const segment = segments.get(detailSegment.value) || current
        const zone = segment.timezone || 'UTC'
        const rows = daily.value.filter(row => segmentKey(row) === segmentKey(segment))
        const dates = rows.map(row => row.date).sort()
        const range = periodRange(detailPeriod.value, zone, dates, report.value?.as_of ?? Date.now())
        const visible = rows.filter(row => row.date >= range.from && row.date <= range.to)
        const totals = summarizeDays(visible)
        return [
          h('h3', '期间估算成本'),
          h('div', { class: 'us-row' }, [
            periodFilter(detailPeriod.value, value => { detailPeriod.value = value }),
            h('label', { class: 'us-field us-filter' }, ['记录分段', select(detailSegment.value, [
              ['', `当前分段 · ${current.timezone}`],
              ...[...segments].map(([key, row]) => [key, `历史分段 · 钱包 ${row.wallet_epoch} / 日历 ${row.statistics_epoch} · ${row.timezone || 'UTC'}`]),
            ], value => {
              detailSegment.value = value
              if (value) detailPeriod.value = 'retained'
            })]),
          ]),
          h('p', { class: 'us-muted' }, `${zone} · ${calendarRange(range)}。估算成本来自相邻钱包样本的余额下降，期间充值可能掩盖消耗。查询时间按浏览器本地时区显示。`),
          table(['原单位', '观察到的下降', '估算成本', '计价覆盖', '覆盖秒数'], totals.map(row => [
            row.unit, amountText(row.upstream_cost, row.unit), costSummary(row), pricingCoverage(row), String(row.covered_seconds),
          ])),
          h('h3', '每日余额与成本'),
          table(['日期 / 原单位', '实测可用余额', '观察到的余额变化', '估算成本', '计价覆盖', '统计质量'], visible.map(row => [
            h('div', { class: 'us-stack' }, [h('strong', row.date), h('small', row.unit)]),
            h('div', { class: 'us-stack' }, [
              h('span', `首笔 ${row.open_balance ?? '—'} / 末笔 ${row.close_balance ?? '—'}`),
              h('span', `最低 ${row.min_balance ?? '—'} / 最高 ${row.max_balance ?? '—'}`),
              h('small', row.last_sample_at ? `最后成功 ${displayTime(row.last_sample_at)}` : '当日无实测余额'),
            ]),
            h('div', { class: 'us-stack' }, [
              h('span', `增加 ${amountText(row.balance_increase)} / 下降 ${amountText(row.upstream_cost)}`),
              h('span', `净变化 ${amountText(row.net_change)}`),
              h('small', `余额下降估算 ${amountText(row.estimated_spend)}`),
              h('small', `账户消费 ${amountText(row.account_spend)}`),
            ]),
            costSummary(row), pricingCoverage(row),
            h('div', { class: 'us-stack' }, [
              h('span', `覆盖 ${row.covered_seconds ?? '未知'} 秒`),
              h('small', [row.gap && '有缺测', row.cross_day && '跨日分摊', row.available_only && '按可用余额'].filter(Boolean).join('、')),
            ]),
          ])),
        ]
      }
      function details() {
        const site = selected.value
        return h(TrCard, null, () => [
          h('h2', `${site.config.name} · 记录`),
          h('div', { class: 'us-actions' }, [
            button('刷新记录', async () => { await refresh(); if (selected.value) await loadDetails(selected.value, offset.value) }),
            ...site.config.probes.map(probe => button(`运行探针 ${probe.id}`, async () => {
              await query(site, probe.id)
              await loadHistory(site, 0)
            })),
          ]),
          h('div', { class: 'us-row' }, [h('strong', '最近已知余额查询'), walletStatus(site)]),
          ...dailySummary(site),
          h('h3', '查询与探针历史'),
          table(['时间', '来源', '结果', '账户余额', '诊断'], history.value.map(observation => [
            displayTime(observation.at), observation.source, statuses[observation.outcome] || observation.outcome,
            (observation.balances || []).map(balance => amountText(balance.available, balance.unit)).join(' / ') || '未知',
            `${observation.code}${observation.http_status ? ` (HTTP ${observation.http_status})` : ''}`,
          ])),
          h('div', { class: 'us-actions' }, [
            button('上一页', () => loadHistory(site, offset.value - 100), true, offset.value === 0),
            h('span', `共 ${historyTotal.value} 条，第 ${Math.floor(offset.value / 100) + 1} 页`),
            button('下一页', () => loadHistory(site, offset.value + 100), true, offset.value + 100 >= historyTotal.value),
          ]),
          h('h3', '自动控制记录'),
          table(['时间', '提供商 ID', '操作', '人工或系统选择'], (control.value.status?.events || []).filter(event => event.site_id === site.id).map(event => [
            displayTime(event.created_at), String(event.provider_id),
            ({ suspend: '插件停用', release: '解除插件停用', external_switch: '人工或系统修改开关' })[event.kind] || event.kind,
            event.manual_schedulable ? '启用' : '停用',
          ])),
          h('h3', '告警记录'),
          table(['时间', '类型', '数值', '通知'], alerts.value.map(alert => [
            displayTime(alert.at || alert.created_at), alert.kind, String(alert.value ?? alert.data?.value ?? ''),
            alert.delivery || alert.delivery_status || '已保存',
          ])),
        ])
      }
      onMounted(() => perform(refresh))
      return () => h('div', { class: 'us-page' }, [h('div', { class: 'us-header' }, [h('div', [h('h1', '上游站点'), h('p', `${sites.value.length} 个站点 · ${sites.value.reduce((n, s) => n + s.provider_count, 0)} 个绑定`)]), h(TrBadge, { tone: 'neutral' }, () => `控制开启：${sites.value.filter(s => s.config.policy.mode !== 'off').length} 个站点 · 插件停用：${control.value.status?.holds?.length || 0} 个提供商`)]), h('div', { class: 'us-actions' }, [button('站点列表', async () => { tab.value = 'sites'; await refresh() }), button('提供商绑定', loadBindings)]), error.value ? h('div', { class: 'us-error', role: 'alert' }, error.value) : null, message.value ? h('div', { class: 'us-notice', role: 'status' }, message.value) : null, loading.value ? h('p', { role: 'status' }, '正在处理…') : null, tab.value === 'edit' && form.value ? editor() : tab.value === 'bindings' ? bindingPage() : tab.value === 'history' && selected.value ? details() : overview()])
    },
  } } })
})()
