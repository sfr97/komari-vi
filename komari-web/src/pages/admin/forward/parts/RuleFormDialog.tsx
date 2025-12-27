import { Button, Card, Dialog, Flex, Grid, RadioCards, Select, Switch, Text, TextArea, TextField } from '@radix-ui/themes'
import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'
import NodeSelectorDialog from '@/components/NodeSelectorDialog'
import AlertConfigCard, { AlertConfig, defaultAlertConfig } from './AlertConfigCard'
import { useNodeDetails, type NodeDetail } from '@/contexts/NodeDetailsContext'
import TestConnectivityDialog from './TestConnectivityDialog'

export type RuleFormState = {
	id?: number
	name: string
	group_name: string
	tags: string
	notes: string
	type: string
	is_enabled: boolean
	config_json: string
}

type RelayForm = {
	node_id: string
	port: string
	sort_order: number
	current_port?: number
}

type HopForm =
	| {
			id: string
			type: 'direct'
			node_id: string
			port: string
			current_port?: number
			sort_order?: number
	  }
	| {
			id: string
			type: 'relay_group'
			relays: RelayForm[]
			strategy: string
			active_relay_node_id?: string
			network?: NetworkConfig
	  }

type StructuredConfig = {
	entry_node_id: string
	entry_port: string
	entry_current_port?: number
	protocol: 'tcp' | 'udp' | 'both'
	target_type: 'custom' | 'node'
	target_node_id?: string
	target_host?: string
	target_port?: string
	relays: RelayForm[]
	strategy: string
	active_relay_node_id?: string
	network?: NetworkConfig
	hops: HopForm[]
}

type Props = {
	open: boolean
	initial: RuleFormState
	onClose: () => void
	onSubmit: (data: RuleFormState) => void
}

const uid = () => (typeof crypto !== 'undefined' && crypto.randomUUID ? crypto.randomUUID() : `hop-${Date.now()}-${Math.random().toString(16).slice(2)}`)

type NetworkConfig = Partial<{
	failover_probe_interval_ms: number
	failover_probe_timeout_ms: number
	failover_failfast_timeout_ms: number
	failover_ok_ttl_ms: number
	failover_backoff_base_ms: number
	failover_backoff_max_ms: number
	failover_retry_window_ms: number
	failover_retry_sleep_ms: number
}>

const normalizeStrategy = (strategy?: string) => {
	const s = (strategy || '').toLowerCase().trim()
	if (!s) return ''
	if (s === 'priority') return 'failover'
	if (['roundrobin', 'iphash', 'failover'].includes(s)) return s
	return 'failover'
}

const normalizeNetwork = (network: any): NetworkConfig | undefined => {
	if (!network || typeof network !== 'object') return undefined
	const toNumber = (v: any): number | undefined => {
		if (typeof v === 'number' && Number.isFinite(v) && Number.isInteger(v) && v >= 0) return v
		if (typeof v === 'string' && v.trim() !== '') {
			const n = Number(v)
			if (Number.isFinite(n) && Number.isInteger(n) && n >= 0) return n
		}
		return undefined
	}
	const keys = [
		'failover_probe_interval_ms',
		'failover_probe_timeout_ms',
		'failover_failfast_timeout_ms',
		'failover_ok_ttl_ms',
		'failover_backoff_base_ms',
		'failover_backoff_max_ms',
		'failover_retry_window_ms',
		'failover_retry_sleep_ms'
	] as const
	const out: Record<string, number> = {}
	for (const k of keys) {
		const n = toNumber((network as any)[k])
		if (n !== undefined) out[k] = n
	}
	return Object.keys(out).length ? (out as NetworkConfig) : undefined
}

const RuleFormDialog = ({ open, initial, onClose, onSubmit }: Props) => {
	const { t } = useTranslation()
	const { nodeDetail } = useNodeDetails()
	const [form, setForm] = useState<RuleFormState>(initial)
	const [saving, setSaving] = useState(false)
	const [mode, setMode] = useState<'structured' | 'raw'>('structured')
	const [checkingPort, setCheckingPort] = useState(false)
	const [alertConfig, setAlertConfig] = useState<AlertConfig>(defaultAlertConfig)
	const [testOpen, setTestOpen] = useState(false)
	const [testConfig, setTestConfig] = useState('')
	const [portChecks, setPortChecks] = useState<Record<string, { status: 'checking' | 'ok' | 'fail'; message?: string; port?: number }>>({})
	const portCheckTimers = useRef<Record<string, number>>({})
	const [structured, setStructured] = useState<StructuredConfig>({
		entry_node_id: '',
		entry_port: '',
		protocol: 'both',
		target_type: 'custom',
		target_node_id: '',
		target_host: '',
		target_port: '',
		relays: [],
		strategy: 'failover',
		active_relay_node_id: '',
		hops: []
	})

	const linuxFilter = (node: NodeDetail) => (node.os || '').toLowerCase().includes('linux')
	const selectedNodeIds = useMemo(() => {
		const ids = new Set<string>()
		if (structured.entry_node_id) ids.add(structured.entry_node_id)
		if (structured.target_type === 'node' && structured.target_node_id) ids.add(structured.target_node_id)
		for (const relay of structured.relays || []) {
			if (relay.node_id) ids.add(relay.node_id)
		}
		for (const hop of structured.hops || []) {
			if (hop.type === 'direct' && hop.node_id) {
				ids.add(hop.node_id)
			}
			if (hop.type === 'relay_group') {
				for (const relay of hop.relays || []) {
					if (relay.node_id) ids.add(relay.node_id)
				}
			}
		}
		return Array.from(ids)
	}, [structured])

	const excludeFor = (current: string[] = []) => {
		const currentSet = new Set(current.filter(Boolean))
		return selectedNodeIds.filter(id => !currentSet.has(id))
	}

	useEffect(() => {
		setForm(initial)
		setPortChecks({})
		try {
			const parsed = JSON.parse(initial.config_json || '{}')
			const cfg: StructuredConfig = {
				entry_node_id: parsed.entry_node_id || '',
				entry_port: parsed.entry_port || '',
				entry_current_port: parsed.entry_current_port || 0,
				protocol: parsed.protocol || 'both',
				target_type: parsed.target_type || 'custom',
				target_node_id: parsed.target_node_id || '',
				target_host: parsed.target_host || '',
				target_port: parsed.target_port?.toString?.() || '',
				relays: (parsed.relays || []).map((r: any, idx: number) => ({
					node_id: r.node_id || '',
					port: r.port || '',
					sort_order: r.sort_order ?? idx + 1,
					current_port: r.current_port || 0
				})),
				strategy: normalizeStrategy(parsed.strategy) || 'failover',
				active_relay_node_id: parsed.active_relay_node_id || '',
				network: normalizeNetwork(parsed.network),
				hops: (parsed.hops || []).map((h: any, idx: number) =>
					h.type === 'relay_group'
						? {
								id: `hop-${idx}`,
								type: 'relay_group' as const,
								relays: (h.relays || []).map((r: any, ridx: number) => ({
									node_id: r.node_id || '',
									port: r.port || '',
									sort_order: r.sort_order ?? ridx + 1,
									current_port: r.current_port || 0
								})),
								strategy: normalizeStrategy(h.strategy) || 'failover',
								active_relay_node_id: h.active_relay_node_id || '',
								network: normalizeNetwork(h.network)
						  }
						: {
								id: `hop-${idx}`,
								type: 'direct' as const,
								node_id: h.node_id || '',
								port: h.port || '',
								current_port: h.current_port || 0,
								sort_order: h.sort_order || idx + 1
						  }
				)
			}
			setStructured(cfg)
			setMode('structured')
		} catch {
			setMode('raw')
		}
	}, [initial])

	// 拉取告警配置
	useEffect(() => {
		const fetchAlert = async () => {
			if (!initial.id) {
				setAlertConfig(defaultAlertConfig)
				return
			}
			try {
				const res = await fetch(`/api/v1/forwards/${initial.id}/alert-config`)
				if (!res.ok) throw new Error(`HTTP ${res.status}`)
				const body = await res.json()
				setAlertConfig({
					enabled: body.data?.enabled ?? false,
					node_down_enabled: body.data?.node_down_enabled ?? true,
					link_degraded_enabled: body.data?.link_degraded_enabled ?? true,
					link_faulty_enabled: body.data?.link_faulty_enabled ?? true,
					high_latency_enabled: body.data?.high_latency_enabled ?? false,
					high_latency_threshold: body.data?.high_latency_threshold ?? 200,
					traffic_spike_enabled: body.data?.traffic_spike_enabled ?? false,
					traffic_spike_threshold: Number(body.data?.traffic_spike_threshold ?? 2)
				})
			} catch (e: any) {
				toast.error(e?.message || 'Load alert config failed')
				setAlertConfig(defaultAlertConfig)
			}
		}
		fetchAlert()
		// eslint-disable-next-line react-hooks/exhaustive-deps
	}, [initial.id])

	useEffect(() => {
		setStructured(prev => ({
			...prev,
			relays: form.type === 'relay_group' ? prev.relays : [],
			hops: form.type === 'chain' ? prev.hops : [],
			strategy: form.type === 'relay_group' ? prev.strategy : prev.strategy || 'failover',
			active_relay_node_id: form.type === 'relay_group' ? prev.active_relay_node_id : ''
		}))
	}, [form.type])

	const buildConfigPayload = () => {
		const cfg: any = {
			entry_node_id: structured.entry_node_id,
			entry_port: structured.entry_port,
			entry_current_port: structured.entry_current_port || 0,
			protocol: structured.protocol,
			target_type: structured.target_type,
			target_node_id: structured.target_type === 'node' ? structured.target_node_id : null,
			target_host: structured.target_type === 'custom' ? structured.target_host : null,
			target_port: Number(structured.target_port) || 0,
			relays: structured.relays.map((r, idx) => ({
				node_id: r.node_id,
				port: r.port,
				current_port: r.current_port || 0,
				sort_order: r.sort_order || idx + 1
			})),
			strategy: normalizeStrategy(structured.strategy) || 'failover',
			active_relay_node_id: structured.active_relay_node_id || '',
			network: normalizeNetwork(structured.network),
			hops: structured.hops.map((h, idx) =>
				h.type === 'relay_group'
					? {
							type: 'relay_group',
							relays: h.relays.map((r, ridx) => ({
								node_id: r.node_id,
								port: r.port,
								current_port: r.current_port || 0,
								sort_order: r.sort_order || ridx + 1
							})),
							strategy: normalizeStrategy(h.strategy) || 'failover',
							active_relay_node_id: h.active_relay_node_id || '',
							network: normalizeNetwork(h.network),
							sort_order: idx + 1
					  }
					: {
							type: 'direct',
							node_id: h.node_id,
							port: h.port,
							current_port: h.current_port || 0,
							sort_order: h.sort_order || idx + 1
					  }
			)
		}
		cfg.type = form.type
		return cfg
	}

	const handleSubmit = async () => {
		setSaving(true)
		try {
			if (mode === 'structured') {
				if (!validateStructured(form.type, structured, t)) {
					return
				}
				const cfg = buildConfigPayload()
				await onSubmit({ ...form, config_json: JSON.stringify(cfg, null, 2) })
				// 保存告警配置（仅编辑时生效）
				if (form.id) {
					await saveAlertConfig(form.id)
				}
			} else {
				await onSubmit(form)
			}
		} finally {
			setSaving(false)
		}
	}

	const openTestConnectivity = () => {
		const config = mode === 'structured' ? JSON.stringify(buildConfigPayload()) : form.config_json
		if (!config) {
			toast.error(t('forward.config') + ' ' + (t('common.required') || 'required'))
			return
		}
		setTestConfig(config)
		setTestOpen(true)
	}

	const generatedPreview = useMemo(() => {
		if (mode !== 'structured') return ''
		return JSON.stringify(buildConfigPayload(), null, 2)
	}, [structured, mode, form.type])

	const formDisabled = saving

	const updateHop = (id: string, updater: (hop: HopForm) => HopForm) => {
		setStructured(prev => {
			const nextHops = prev.hops.map(h => (h.id === id ? updater(h) : h))
			return { ...prev, hops: nextHops }
		})
	}

	const checkPortStatus = async (nodeId: string, portSpec: string) => {
		const res = await fetch('/api/v1/forwards/check-port', {
			method: 'POST',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify({ rule_id: initial.id, node_id: nodeId, port_spec: portSpec })
		})
		if (!res.ok) throw new Error(`HTTP ${res.status}`)
		const body = await res.json()
		return body.data || null
	}

	const checkPort = async (nodeId: string, portSpec: string, onOk: (val: number) => void) => {
		if (formDisabled) return
		if (!nodeId || !portSpec) {
			toast.error(t('forward.portCheckNeedNode') || '请选择节点与端口')
			return
		}
		if (checkingPort) return
		setCheckingPort(true)
		try {
			const result = await checkPortStatus(nodeId, portSpec)
			if (result?.success && result.available_port) {
				onOk(result.available_port)
				toast.success(t('forward.portCheckSuccess', { port: result.available_port }))
			} else {
				toast.error(result?.message || t('forward.portCheckFailed'))
			}
		} catch (e: any) {
			toast.error(e?.message || 'Check failed')
		} finally {
			setCheckingPort(false)
		}
	}

	const schedulePortCheck = (key: string, nodeId: string, portSpec: string) => {
		if (!nodeId || !portSpec) {
			setPortChecks(prev => {
				const next = { ...prev }
				delete next[key]
				return next
			})
			return
		}
		if (portCheckTimers.current[key]) {
			clearTimeout(portCheckTimers.current[key])
		}
		portCheckTimers.current[key] = window.setTimeout(async () => {
			setPortChecks(prev => ({ ...prev, [key]: { status: 'checking' } }))
			try {
				const result = await checkPortStatus(nodeId, portSpec)
				if (result?.success && result.available_port) {
					setPortChecks(prev => ({
						...prev,
						[key]: { status: 'ok', port: result.available_port, message: result.message }
					}))
				} else {
					setPortChecks(prev => ({
						...prev,
						[key]: { status: 'fail', message: result?.message || t('forward.portCheckFailed') }
					}))
				}
			} catch (e: any) {
				setPortChecks(prev => ({
					...prev,
					[key]: { status: 'fail', message: e?.message || t('forward.portCheckFailed') }
				}))
			}
		}, 600)
	}

	useEffect(() => {
		return () => {
			Object.values(portCheckTimers.current).forEach(timer => clearTimeout(timer))
		}
	}, [])

	const renderPortCheck = (key: string) => {
		const state = portChecks[key]
		if (!state) return null
		if (state.status === 'checking') {
			return (
				<Text size="1" color="gray">
					{t('forward.checkingPort')}
				</Text>
			)
		}
		if (state.status === 'ok') {
			return (
				<Text size="1" color="green">
					{t('forward.portCheckSuccess', { port: state.port })}
				</Text>
			)
		}
		return (
			<Text size="1" color="red">
				{state.message || t('forward.portCheckFailed')}
			</Text>
		)
	}

	const saveAlertConfig = async (id: number) => {
		try {
			const res = await fetch(`/api/v1/forwards/${id}/alert-config`, {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({
					enabled: alertConfig.enabled,
					node_down_enabled: alertConfig.node_down_enabled,
					link_degraded_enabled: alertConfig.link_degraded_enabled,
					link_faulty_enabled: alertConfig.link_faulty_enabled,
					high_latency_enabled: alertConfig.high_latency_enabled,
					high_latency_threshold: alertConfig.high_latency_threshold,
					traffic_spike_enabled: alertConfig.traffic_spike_enabled,
					traffic_spike_threshold: alertConfig.traffic_spike_threshold
				})
			})
			if (!res.ok) throw new Error(`HTTP ${res.status}`)
		} catch (e: any) {
			toast.error(e?.message || 'Save alert config failed')
		}
	}

	return (
		<>
			<Dialog.Root open={open} onOpenChange={isOpen => (!isOpen ? onClose() : null)}>
				<Dialog.Content maxWidth="820px">
				<Dialog.Title>{form.id ? t('forward.edit') : t('forward.create')}</Dialog.Title>
				<Grid columns="2" gap="3" mt="3">
					<div className="col-span-1 flex flex-col gap-2">
						<Text>{t('forward.name')}</Text>
						<TextField.Root value={form.name} onChange={e => setForm({ ...form, name: e.target.value })} disabled={formDisabled} />
					</div>
					<div className="col-span-1 flex flex-col gap-2">
						<Text>{t('forward.group')}</Text>
						<TextField.Root value={form.group_name} onChange={e => setForm({ ...form, group_name: e.target.value })} disabled={formDisabled} />
					</div>
					<div className="col-span-1 flex flex-col gap-2">
						<Text>{t('forward.tags')}</Text>
						<TextField.Root
							value={form.tags}
							onChange={e => setForm({ ...form, tags: e.target.value })}
							placeholder={t('forward.tagsPlaceholder', { defaultValue: '例如: tcp, 高可用' })}
							disabled={formDisabled}
						/>
					</div>
					<div className="col-span-1 flex flex-col gap-2">
						<Text>{t('forward.type')}</Text>
						<Select.Root
							value={form.type}
							onValueChange={v => {
								setForm({ ...form, type: v })
								setMode('structured')
							}}
							disabled={formDisabled}>
							<Select.Trigger />
							<Select.Content>
								<Select.Item value="direct">{t('forward.typeDirect', { defaultValue: '中转' })}</Select.Item>
								<Select.Item value="relay_group">{t('forward.typeRelayGroup', { defaultValue: '中继组' })}</Select.Item>
								<Select.Item value="chain">{t('forward.typeChain', { defaultValue: '链式' })}</Select.Item>
							</Select.Content>
						</Select.Root>
					</div>
					<div className="col-span-1 flex items-center gap-2">
						<Text>{t('forward.enabled')}</Text>
						<Switch checked={form.is_enabled} onCheckedChange={checked => setForm({ ...form, is_enabled: Boolean(checked) })} disabled={formDisabled} />
					</div>
					<div className="col-span-2 flex flex-col gap-2">
						<Text>{t('forward.notes')}</Text>
						<TextArea value={form.notes} onChange={e => setForm({ ...form, notes: e.target.value })} disabled={formDisabled} />
					</div>
				</Grid>
				<div className="mt-3">
					<RadioCards.Root value={mode} onValueChange={v => (!formDisabled ? setMode(v as 'structured' | 'raw') : null)}>
						<RadioCards.Item value="structured">{t('forward.modeStructured')}</RadioCards.Item>
						<RadioCards.Item value="raw">{t('forward.modeRaw')}</RadioCards.Item>
					</RadioCards.Root>
				</div>
				{mode === 'structured' ? (
					<div className="mt-3 space-y-3">
						<Grid columns="2" gap="3">
							<div className="flex flex-col gap-2">
								<Text>{t('forward.entry')}</Text>
								<NodeSelectorDialog
									value={structured.entry_node_id ? [structured.entry_node_id] : []}
									onChange={ids => {
										const nextId = ids[0] || ''
										setStructured(prev => ({ ...prev, entry_node_id: nextId }))
										schedulePortCheck('entry', nextId, structured.entry_port)
									}}
									title={t('forward.entry')}
									hiddenDescription
									showViewModeToggle
									disabled={formDisabled}
									filterNode={linuxFilter}
									excludeIds={excludeFor([structured.entry_node_id])}
								/>
								{structured.entry_node_id && <Text size="1" color="gray">UUID: {structured.entry_node_id}</Text>}
							</div>
							<div className="flex flex-col gap-2">
								<Text>{t('forward.entryPort')}</Text>
								<Flex gap="2" align="center">
									<TextField.Root
										value={structured.entry_port}
										onChange={e => {
											const value = e.target.value
											setStructured(prev => ({ ...prev, entry_port: value }))
											schedulePortCheck('entry', structured.entry_node_id, value)
										}}
										placeholder="8881 或 8000-9000"
										disabled={formDisabled}
									/>
									<Button
										variant="soft"
										disabled={checkingPort || formDisabled}
										onClick={() =>
											checkPort(structured.entry_node_id, structured.entry_port, val =>
												{
													setStructured(prev => ({ ...prev, entry_port: `${val}`, entry_current_port: val }))
													schedulePortCheck('entry', structured.entry_node_id, `${val}`)
												}
											)
										}>
										{checkingPort ? t('forward.checkingPort') : t('forward.checkPort')}
									</Button>
								</Flex>
								{renderPortCheck('entry')}
							</div>
							<div className="flex flex-col gap-2">
								<Text>{t('forward.protocol') || 'Protocol'}</Text>
								<Select.Root
									value={structured.protocol}
									onValueChange={v => setStructured(prev => ({ ...prev, protocol: v as 'tcp' | 'udp' | 'both' }))}
									disabled={formDisabled}>
									<Select.Trigger />
									<Select.Content>
										<Select.Item value="tcp">tcp</Select.Item>
										<Select.Item value="udp">udp</Select.Item>
										<Select.Item value="both">both</Select.Item>
									</Select.Content>
								</Select.Root>
							</div>
						</Grid>

						<Card>
							<Text weight="bold" mb="2">
								{t('forward.config')}
							</Text>
							{form.type === 'direct' && (
								<Grid columns="2" gap="3">
									<TargetSelector
										structured={structured}
										setStructured={setStructured}
										disabled={formDisabled}
										filterNode={linuxFilter}
										excludeIds={excludeFor([structured.target_node_id])}
									/>
								</Grid>
							)}
							{form.type === 'relay_group' && (
								<div className="space-y-3">
									<RelayEditor
										relays={structured.relays}
										onChange={relays => setStructured(prev => ({ ...prev, relays }))}
										checkPort={checkPort}
										disabled={formDisabled}
										filterNode={linuxFilter}
										excludeFor={excludeFor}
										schedulePortCheck={schedulePortCheck}
										renderPortCheck={renderPortCheck}
									/>
									<div className="flex flex-col gap-2">
										<Text>{t('forward.strategy')}</Text>
									<Select.Root
											value={structured.strategy}
											onValueChange={v => setStructured(prev => ({ ...prev, strategy: v }))}
											disabled={formDisabled}>
											<Select.Trigger />
											<Select.Content>
												<Select.Item value="failover">{t('forward.strategyFailover', { defaultValue: '故障转移' })}</Select.Item>
												<Select.Item value="roundrobin">{t('forward.strategyRoundRobin', { defaultValue: '轮询' })}</Select.Item>
												<Select.Item value="iphash">{t('forward.strategyIPHash', { defaultValue: 'IP Hash' })}</Select.Item>
											</Select.Content>
										</Select.Root>
									</div>
									{(structured.strategy || '').toLowerCase() === 'failover' && (
										<FailoverNetworkEditor
											value={structured.network}
											onChange={net => setStructured(prev => ({ ...prev, network: net }))}
											disabled={formDisabled}
										/>
									)}
									<Grid columns="2" gap="3">
										<TargetSelector
											structured={structured}
											setStructured={setStructured}
											disabled={formDisabled}
											filterNode={linuxFilter}
											excludeIds={excludeFor([structured.target_node_id])}
										/>
									</Grid>
								</div>
							)}
							{form.type === 'chain' && (
								<div className="space-y-3">
									<div className="flex gap-2">
									<Button
										variant="soft"
										disabled={formDisabled}
										onClick={() =>
											setStructured(prev => ({
												...prev,
												hops: [...prev.hops, { id: uid(), type: 'direct', node_id: '', port: '', current_port: 0 }]
											}))
										}>
										{t('forward.addDirectHop')}
									</Button>
									<Button
										variant="soft"
										disabled={formDisabled}
										onClick={() =>
											setStructured(prev => ({
												...prev,
												hops: [
													...prev.hops,
													{
														id: uid(),
														type: 'relay_group',
														relays: [],
														strategy: 'failover',
														active_relay_node_id: ''
													}
													]
												}))
											}>
											{t('forward.addRelayHop')}
										</Button>
									</div>
									{structured.hops.map(hop =>
										hop.type === 'direct' ? (
											<Card key={hop.id} className="p-3 space-y-2">
												<Flex justify="between" align="center">
													<Text weight="bold">{t('forward.directHop')}</Text>
													<Button
														variant="ghost"
														color="red"
														disabled={formDisabled}
														onClick={() => setStructured(prev => ({ ...prev, hops: prev.hops.filter(h => h.id !== hop.id) }))}>
														{t('common.delete')}
													</Button>
												</Flex>
												<Grid columns="2" gap="3">
													<div className="flex flex-col gap-2">
														<Text>{t('forward.targetNode')}</Text>
														<NodeSelectorDialog
															value={hop.node_id ? [hop.node_id] : []}
															onChange={ids => {
																const nextId = ids[0] || ''
																updateHop(hop.id, h => ({ ...h, node_id: nextId } as HopForm))
																schedulePortCheck(`hop-${hop.id}`, nextId, hop.port)
															}}
															title={t('forward.targetNode')}
															hiddenDescription
															showViewModeToggle
															disabled={formDisabled}
															filterNode={linuxFilter}
															excludeIds={excludeFor([hop.node_id])}
														/>
														{hop.node_id && <Text size="1">UUID: {hop.node_id}</Text>}
													</div>
													<div className="flex flex-col gap-2">
														<Text>{t('forward.targetPort')}</Text>
														<Flex gap="2">
															<TextField.Root
																value={hop.port}
																onChange={e => {
																	const value = e.target.value
																	updateHop(hop.id, h => ({ ...h, port: value } as HopForm))
																	schedulePortCheck(`hop-${hop.id}`, hop.node_id, value)
																}}
																placeholder="10000-20000"
																disabled={formDisabled}
															/>
															<Button
																variant="soft"
																disabled={checkingPort || formDisabled}
																onClick={() =>
																	checkPort(
																		hop.node_id,
																		hop.port,
																		val => {
																			updateHop(hop.id, h => ({ ...h, port: `${val}`, current_port: val } as HopForm))
																			schedulePortCheck(`hop-${hop.id}`, hop.node_id, `${val}`)
																		}
																	)
																}>
																{checkingPort ? t('forward.checkingPort') : t('forward.checkPort')}
															</Button>
														</Flex>
														{renderPortCheck(`hop-${hop.id}`)}
													</div>
												</Grid>
											</Card>
										) : (
											<Card key={hop.id} className="p-3 space-y-2">
												<Flex justify="between" align="center">
													<Text weight="bold">{t('forward.relayGroup')}</Text>
													<Button
														variant="ghost"
														color="red"
														disabled={formDisabled}
														onClick={() => setStructured(prev => ({ ...prev, hops: prev.hops.filter(h => h.id !== hop.id) }))}>
														{t('common.delete')}
													</Button>
												</Flex>
												<RelayEditor
													relays={hop.relays}
													onChange={relays => updateHop(hop.id, h => ({ ...(h as any), relays } as HopForm))}
													checkPort={checkPort}
													disabled={formDisabled}
													filterNode={linuxFilter}
													excludeFor={excludeFor}
													schedulePortCheck={(key, nodeId, portSpec) => schedulePortCheck(`hop-${hop.id}-${key}`, nodeId, portSpec)}
													renderPortCheck={key => renderPortCheck(`hop-${hop.id}-${key}`)}
												/>
												<div className="flex flex-col gap-2">
													<Text>{t('forward.strategy')}</Text>
														<Select.Root
															value={hop.strategy}
															onValueChange={v => updateHop(hop.id, h => ({ ...(h as any), strategy: v } as HopForm))}
															disabled={formDisabled}>
															<Select.Trigger />
															<Select.Content>
																<Select.Item value="failover">{t('forward.strategyFailover', { defaultValue: '故障转移' })}</Select.Item>
																<Select.Item value="roundrobin">{t('forward.strategyRoundRobin', { defaultValue: '轮询' })}</Select.Item>
																<Select.Item value="iphash">{t('forward.strategyIPHash', { defaultValue: 'IP Hash' })}</Select.Item>
															</Select.Content>
														</Select.Root>
												</div>
												{(hop.strategy || '').toLowerCase() === 'failover' && (
													<FailoverNetworkEditor
														value={hop.network}
														onChange={net => updateHop(hop.id, h => ({ ...(h as any), network: net } as HopForm))}
														disabled={formDisabled}
													/>
												)}
											</Card>
										)
									)}
									<Grid columns="2" gap="3">
										<TargetSelector
											structured={structured}
											setStructured={setStructured}
											disabled={formDisabled}
											filterNode={linuxFilter}
											excludeIds={excludeFor([structured.target_node_id])}
										/>
									</Grid>
								</div>
							)}
						</Card>

						<div className="flex flex-col gap-2">
							<Text size="2" color="gray">
								{`Preview (config_json, type: ${form.type || 'direct'})`}
							</Text>
							<TextArea value={generatedPreview} readOnly minRows={6} />
						</div>
						<AlertConfigCard value={alertConfig} onChange={setAlertConfig} collapsible defaultOpen={false} />
					</div>
				) : (
					<div className="col-span-2 flex flex-col gap-2 mt-3">
						<Text>{t('forward.config')}</Text>
						<TextArea
							minRows={10}
							value={form.config_json}
							onChange={e => setForm({ ...form, config_json: e.target.value })}
							placeholder={t('forward.configPlaceholder')}
							disabled={formDisabled}
						/>
					</div>
				)}
				<div className="mt-4 flex justify-end gap-3">
					<Button variant="soft" onClick={openTestConnectivity} disabled={saving}>
						{t('forward.testConnectivity')}
					</Button>
					<Button variant="soft" onClick={onClose}>
						{t('forward.cancel')}
					</Button>
					<Button onClick={handleSubmit} disabled={saving}>
						{t('forward.submit')}
					</Button>
				</div>
				</Dialog.Content>
			</Dialog.Root>
			<TestConnectivityDialog open={testOpen} configJson={testConfig} onClose={() => setTestOpen(false)} />
		</>
	)
}

const TargetSelector = ({
	structured,
	setStructured,
	disabled = false,
	filterNode,
	excludeIds = []
}: {
	structured: StructuredConfig
	setStructured: React.Dispatch<React.SetStateAction<StructuredConfig>>
	disabled?: boolean
	filterNode?: (node: NodeDetail) => boolean
	excludeIds?: string[]
}) => {
	const { t } = useTranslation()
	return (
		<>
			<div className="flex flex-col gap-2">
				<Text>{t('forward.target')}</Text>
				<Select.Root
					value={structured.target_type}
					onValueChange={v => setStructured(prev => ({ ...prev, target_type: v as 'node' | 'custom' }))}
					disabled={disabled}>
					<Select.Trigger />
					<Select.Content>
						<Select.Item value="custom">{t('forward.target')}</Select.Item>
						<Select.Item value="node">{t('forward.targetNode') || 'Node'}</Select.Item>
					</Select.Content>
				</Select.Root>
			</div>
			{structured.target_type === 'node' ? (
				<div className="flex flex-col gap-2">
					<Text>{t('forward.targetNode')}</Text>
					<NodeSelectorDialog
						value={structured.target_node_id ? [structured.target_node_id] : []}
						onChange={ids => setStructured(prev => ({ ...prev, target_node_id: ids[0] || '' }))}
						title={t('forward.targetNode') || 'Target Node'}
						hiddenDescription
						showViewModeToggle
						disabled={disabled}
						filterNode={filterNode}
						excludeIds={excludeIds}
					/>
					{structured.target_node_id && <Text size="1" color="gray">UUID: {structured.target_node_id}</Text>}
				</div>
			) : (
				<>
					<div className="flex flex-col gap-2">
						<Text>{t('forward.targetHost')}</Text>
						<TextField.Root
							value={structured.target_host}
							onChange={e => setStructured(prev => ({ ...prev, target_host: e.target.value }))}
							placeholder="1.1.1.1"
							disabled={disabled}
						/>
					</div>
					<div className="flex flex-col gap-2">
						<Text>{t('forward.targetPort')}</Text>
						<TextField.Root
							value={structured.target_port}
							onChange={e => setStructured(prev => ({ ...prev, target_port: e.target.value }))}
							placeholder="53"
							disabled={disabled}
						/>
					</div>
				</>
			)}
		</>
	)
}

const FailoverNetworkEditor = ({ value, onChange, disabled }: { value?: NetworkConfig; onChange: (v?: NetworkConfig) => void; disabled?: boolean }) => {
	const { t } = useTranslation()
	const setField = (key: keyof NetworkConfig, raw: string) => {
		const trimmed = raw.trim()
		const next: any = { ...(value || {}) }
		if (trimmed === '') {
			delete next[key]
		} else {
			const n = Number(trimmed)
			if (Number.isFinite(n) && Number.isInteger(n) && n >= 0) next[key] = n
		}
		onChange(Object.keys(next).length ? (next as NetworkConfig) : undefined)
	}
	const getVal = (key: keyof NetworkConfig) => {
		const v = (value as any)?.[key]
		return typeof v === 'number' && Number.isFinite(v) ? String(v) : ''
	}
	const fields: { key: keyof NetworkConfig; label: string }[] = [
		{ key: 'failover_probe_interval_ms', label: t('forward.failoverProbeInterval', { defaultValue: '探测间隔(ms)' }) },
		{ key: 'failover_probe_timeout_ms', label: t('forward.failoverProbeTimeout', { defaultValue: '探测超时(ms)' }) },
		{ key: 'failover_failfast_timeout_ms', label: t('forward.failoverFailfastTimeout', { defaultValue: 'Failfast 超时(ms)' }) },
		{ key: 'failover_ok_ttl_ms', label: t('forward.failoverOkTTL', { defaultValue: 'OK TTL(ms)' }) },
		{ key: 'failover_backoff_base_ms', label: t('forward.failoverBackoffBase', { defaultValue: '退避基准(ms)' }) },
		{ key: 'failover_backoff_max_ms', label: t('forward.failoverBackoffMax', { defaultValue: '退避上限(ms)' }) },
		{ key: 'failover_retry_window_ms', label: t('forward.failoverRetryWindow', { defaultValue: '重试窗口(ms)' }) },
		{ key: 'failover_retry_sleep_ms', label: t('forward.failoverRetrySleep', { defaultValue: '重试睡眠(ms)' }) }
	]

	return (
		<Card className="p-3">
			<Text weight="bold" mb="2">{t('forward.failoverParams', { defaultValue: 'Failover 参数' })}</Text>
			<Grid columns="2" gap="2">
				{fields.map(f => (
					<div key={String(f.key)} className="flex flex-col gap-2">
						<Text size="1" color="gray">{f.label}</Text>
						<TextField.Root value={getVal(f.key)} onChange={e => setField(f.key, e.target.value)} placeholder="(可选)" disabled={disabled} />
					</div>
				))}
			</Grid>
		</Card>
	)
}

const RelayEditor = ({
	relays,
	onChange,
	checkPort,
	disabled = false,
	filterNode,
	excludeFor,
	schedulePortCheck,
	renderPortCheck
}: {
	relays: RelayForm[]
	onChange: (relays: RelayForm[]) => void
	checkPort: (nodeId: string, portSpec: string, onOk: (val: number) => void) => void
	disabled?: boolean
	filterNode?: (node: NodeDetail) => boolean
	excludeFor?: (current: string[]) => string[]
	schedulePortCheck?: (key: string, nodeId: string, portSpec: string) => void
	renderPortCheck?: (key: string) => ReactNode
}) => {
	const { t } = useTranslation()
	const addRelays = (ids: string[]) => {
		if (!ids?.length) return
		const existing = new Set(relays.map(r => r.node_id))
		const filtered = ids.filter(id => !existing.has(id))
		if (!filtered.length) return
		onChange([
			...relays,
			...filtered.map((id, idx) => ({
				node_id: id,
				port: '',
				sort_order: relays.length + idx + 1
			}))
		])
	}
	return (
		<div className="space-y-2">
			<Flex justify="between" align="center">
				<Text>{t('forward.relayNodes')}</Text>
				<NodeSelectorDialog
					value={[]}
					onChange={addRelays}
					title={t('forward.addRelay')}
					hiddenDescription
					showViewModeToggle
					disabled={disabled}
					filterNode={filterNode}
					excludeIds={excludeFor ? excludeFor([]) : []}>
					<Button variant="soft" disabled={disabled}>
						{t('forward.addRelay')}
					</Button>
				</NodeSelectorDialog>
			</Flex>
			{relays.length === 0 ? (
				<Text color="gray">{t('forward.noRelay')}</Text>
			) : (
				<div className="space-y-2">
					{relays.map((relay, idx) => (
						<Card key={`${relay.node_id}-${idx}`} className="p-3 space-y-2">
							<Flex justify="between" align="center">
								<Text>{relay.node_id || t('forward.relayNodes')}</Text>
								<Button variant="ghost" color="red" disabled={disabled} onClick={() => onChange(relays.filter((_, i) => i !== idx))}>
									{t('common.delete')}
								</Button>
							</Flex>
							<Grid columns="3" gap="2">
								<div className="flex flex-col gap-2">
									<Text>{t('forward.targetNode')}</Text>
									<NodeSelectorDialog
										value={relay.node_id ? [relay.node_id] : []}
										onChange={ids => {
											const id = ids[0] || ''
											onChange(relays.map((r, i) => (i === idx ? { ...r, node_id: id } : r)))
											schedulePortCheck?.(`relay-${idx}`, id, relays[idx]?.port || '')
										}}
										title={t('forward.targetNode')}
										hiddenDescription
										showViewModeToggle
										disabled={disabled}
										filterNode={filterNode}
										excludeIds={excludeFor ? excludeFor([relay.node_id]) : []}
									/>
								</div>
								<div className="flex flex-col gap-2">
									<Text>{t('forward.targetPort')}</Text>
									<Flex gap="2">
									<TextField.Root
										value={relay.port}
										onChange={e => {
											const value = e.target.value
											onChange(relays.map((r, i) => (i === idx ? { ...r, port: value } : r)))
											schedulePortCheck?.(`relay-${idx}`, relay.node_id, value)
										}}
										placeholder="15000 或 10000-20000"
										disabled={disabled}
									/>
										<Button
											variant="soft"
											disabled={checkingPort || disabled}
											onClick={() =>
												checkPort(
													relay.node_id,
													relay.port,
													val => {
														onChange(relays.map((r, i) => (i === idx ? { ...r, port: `${val}`, current_port: val } : r)))
														schedulePortCheck?.(`relay-${idx}`, relay.node_id, `${val}`)
													}
												)
											}>
											{checkingPort ? t('forward.checkingPort') : t('forward.checkPort')}
										</Button>
									</Flex>
									{renderPortCheck?.(`relay-${idx}`)}
								</div>
								<div className="flex flex-col gap-2">
									<Text>{t('forward.sortOrder')}</Text>
									<TextField.Root
										type="number"
										value={relay.sort_order}
										onChange={e =>
											onChange(relays.map((r, i) => (i === idx ? { ...r, sort_order: Number(e.target.value) } : r)))
										}
										disabled={disabled}
									/>
								</div>
							</Grid>
						</Card>
					))}
				</div>
			)}
		</div>
	)
}

	const validateStructured = (type: string, cfg: StructuredConfig, t: any) => {
		if (!cfg.entry_node_id) {
			toast.error(t('forward.entry') + ' ' + (t('common.required') || 'required'))
			return false
		}
	if (!cfg.entry_port) {
		toast.error(t('forward.entryPort') + ' ' + (t('common.required') || 'required'))
		return false
	}
	const checkTarget = () => {
		if (cfg.target_type === 'node') {
			if (!cfg.target_node_id) {
				toast.error(t('forward.targetNode') + ' ' + (t('common.required') || 'required'))
				return false
			}
		} else {
			if (!cfg.target_host || !cfg.target_port) {
				toast.error(t('forward.target') + ' ' + (t('common.required') || 'required'))
				return false
			}
		}
		return true
	}

		if (type === 'direct') {
			return checkTarget()
		}

	if (type === 'relay_group') {
		if (!cfg.relays?.length) {
			toast.error(t('forward.relayNodes') + ' ' + (t('common.required') || 'required'))
			return false
		}
			for (const r of cfg.relays) {
				if (!r.node_id || !r.port) {
					toast.error(t('forward.relayNodes') + ' ' + (t('common.required') || 'required'))
					return false
				}
			}
			return checkTarget()
		}

	if (type === 'chain') {
		if (!cfg.hops?.length) {
			toast.error(t('forward.addRelayHop'))
			return false
		}
		for (const hop of cfg.hops) {
			if (hop.type === 'direct') {
				if (!hop.node_id || !hop.port) {
					toast.error(t('forward.directHop') + ' ' + (t('common.required') || 'required'))
					return false
				}
			} else if (hop.type === 'relay_group') {
				if (!hop.relays?.length) {
					toast.error(t('forward.relayGroup') + ' ' + (t('common.required') || 'required'))
					return false
				}
				for (const r of hop.relays) {
					if (!r.node_id || !r.port) {
						toast.error(t('forward.relayGroup') + ' ' + (t('common.required') || 'required'))
						return false
					}
				}
				}
			}
			return checkTarget()
		}
		return true
	}

export default RuleFormDialog
