import { useEffect, useMemo, useRef, useState } from 'react'
import { useParams, useNavigate } from 'react-router-dom'
import { Badge, Button, Card, Flex, Grid, Table, Text } from '@radix-ui/themes'
import { useTranslation } from 'react-i18next'
import { ArrowLeftIcon } from '@radix-ui/react-icons'
import { LineChart, Line, XAxis, YAxis, CartesianGrid, BarChart, Bar, Tooltip } from 'recharts'
import { ChartContainer, ChartTooltip, ChartTooltipContent } from '@/components/ui/chart'
import { NodeDetailsProvider, useNodeDetails } from '@/contexts/NodeDetailsContext'
import InstanceConnectionsDialog from './parts/InstanceConnectionsDialog'

type ForwardStat = {
	node_id: string
	link_status: string
	active_connections: number
	traffic_in_bytes: number
	traffic_out_bytes: number
	realtime_bps_in: number
	realtime_bps_out: number
	nodes_latency: string
	active_relay_node_id?: string
}

type ForwardHistory = {
	timestamp: string
	traffic_in_bytes: number
	traffic_out_bytes: number
}

type PlannedInstance = {
	instance_id: string
	node_id: string
	listen: string
	listen_port: number
	remote: string
	extra_remotes?: string[]
	balance?: string
}

type ForwardInstanceStats = {
	rule_id: number
	node_id: string
	instance_id: string
	listen: string
	listen_port: number
	stats: any
	route?: any
	last_updated_at: string
}

type TopologyNode = {
	node_id: string
	name: string
	ip: string
	port?: number
	status?: string
	latency_ms?: number
	role: string
}

type Topology = {
	entry: TopologyNode
	relays: TopologyNode[]
	hops: { type: string; strategy?: string; relays?: TopologyNode[]; node?: TopologyNode; active_relay_node_id?: string }[]
	target: TopologyNode
	active_relay_node_id?: string
	type: string
}

type AlertHistory = {
	id: number
	alert_type: string
	severity: string
	message: string
	acknowledged: boolean
	created_at: string
}

const ForwardDashboardInner = () => {
	const { t } = useTranslation()
	const navigate = useNavigate()
	const { id } = useParams()
	const ruleId = Number(id || 0)
	const { nodeDetail } = useNodeDetails()
	const [stats, setStats] = useState<ForwardStat[]>([])
	const [history, setHistory] = useState<ForwardHistory[]>([])
	const [topology, setTopology] = useState<Topology | null>(null)
	const [totals, setTotals] = useState({ connections: 0, in: 0, out: 0 })
	const [alerts, setAlerts] = useState<AlertHistory[]>([])
	const [entryStat, setEntryStat] = useState<ForwardStat | null>(null)
	const [trafficSeries, setTrafficSeries] = useState<{ time: string; in: number; out: number }[]>([])
	const lastTrafficRef = useRef<{ ts: number; inBytes: number; outBytes: number } | null>(null)
	const [instances, setInstances] = useState<PlannedInstance[]>([])
	const [statsByInstance, setStatsByInstance] = useState<Record<string, ForwardInstanceStats>>({})
	const [routeOverride, setRouteOverride] = useState<Record<string, any>>({})
	const [connectionsInstance, setConnectionsInstance] = useState<string | null>(null)

	const fetchStats = async () => {
		if (!ruleId) return
		const res = await fetch(`/api/v1/forwards/${ruleId}/stats`)
		if (!res.ok) throw new Error(`HTTP ${res.status}`)
		const body = await res.json()
		const nextStats = body.data?.stats || []
		const nextHistory = body.data?.history || []
		const nextEntryStat = body.data?.entry_status || null
		setStats(nextStats)
		setHistory(nextHistory)
		setEntryStat(nextEntryStat)
		setTotals({
			connections: body.data?.total_connections || 0,
			in: body.data?.total_traffic_in || 0,
			out: body.data?.total_traffic_out || 0
		})

		// realtime bps 字段在新链路中可能不再由 agent 直接上报；这里用累计 bytes 的增量估算实时 Mbps
		const now = Date.now()
		const inBytes = Number(nextEntryStat?.traffic_in_bytes || 0)
		const outBytes = Number(nextEntryStat?.traffic_out_bytes || 0)
		if (inBytes > 0 || outBytes > 0) {
			const prev = lastTrafficRef.current
			lastTrafficRef.current = { ts: now, inBytes, outBytes }
			if (prev && now > prev.ts) {
				const dt = (now - prev.ts) / 1000
				if (dt > 0.5 && dt < 120) {
					const dIn = Math.max(0, inBytes - prev.inBytes)
					const dOut = Math.max(0, outBytes - prev.outBytes)
					const bpsIn = (dIn * 8) / dt
					const bpsOut = (dOut * 8) / dt
					const point = { time: new Date(now).toLocaleTimeString(), in: bpsIn / 1_000_000, out: bpsOut / 1_000_000 }
					setTrafficSeries(prevSeries => {
						const next = [...prevSeries, point]
						return next.length > 60 ? next.slice(next.length - 60) : next
					})
				}
			}
		}
	}

	const fetchTopology = async () => {
		if (!ruleId) return
		const res = await fetch(`/api/v1/forwards/${ruleId}/topology`)
		if (!res.ok) throw new Error(`HTTP ${res.status}`)
		const body = await res.json()
		setTopology(body.data || null)
	}

	const fetchAlerts = async () => {
		if (!ruleId) return
		const res = await fetch(`/api/v1/forwards/${ruleId}/alert-history?limit=20`)
		if (!res.ok) return
		const body = await res.json()
		setAlerts(body.data || [])
	}

	const fetchInstances = async () => {
		if (!ruleId) return
		const res = await fetch(`/api/v1/forwards/${ruleId}/instances`)
		if (!res.ok) throw new Error(`HTTP ${res.status}`)
		const body = await res.json()
		setInstances(body.data?.instances || [])
		setStatsByInstance(body.data?.stats_by_instance || {})
	}

	useEffect(() => {
		if (!ruleId) return
		const load = async () => {
			try {
				await Promise.all([fetchStats(), fetchTopology(), fetchAlerts(), fetchInstances()])
			} catch {
				// ignore
			}
		}
		load()
		const timer = setInterval(() => {
			fetchStats()
			fetchTopology()
			fetchInstances()
		}, 5000)
		return () => clearInterval(timer)
		// eslint-disable-next-line react-hooks/exhaustive-deps
	}, [ruleId])

	useEffect(() => {
		if (trafficSeries.length > 0) return
		if (!history.length) return
		let bucketSeconds = 60
		if (history.length >= 2) {
			const a = new Date(history[history.length - 2].timestamp).getTime()
			const b = new Date(history[history.length - 1].timestamp).getTime()
			const diff = Math.max(1, Math.round((b - a) / 1000))
			// 限制合理区间，避免异常数据导致图表比例失真
			bucketSeconds = Math.min(24 * 3600, Math.max(10, diff))
		}
		const points = history.slice(Math.max(0, history.length - 60)).map(item => {
			const ts = new Date(item.timestamp)
			const inMbps = (Number(item.traffic_in_bytes || 0) * 8) / bucketSeconds / 1_000_000
			const outMbps = (Number(item.traffic_out_bytes || 0) * 8) / bucketSeconds / 1_000_000
			return { time: ts.toLocaleTimeString(), in: inMbps, out: outMbps }
		})
		setTrafficSeries(points)
	}, [history, trafficSeries.length])

	const formatBytes = (bytes?: number) => {
		if (!bytes) return '0 B'
		const units = ['B', 'KB', 'MB', 'GB', 'TB']
		let idx = 0
		let value = bytes
		while (value >= 1024 && idx < units.length - 1) {
			value /= 1024
			idx++
		}
		const fixed = value >= 10 || idx === 0 ? 0 : 1
		return `${value.toFixed(fixed)} ${units[idx]}`
	}

	const nodeName = (nodeId?: string) => {
		if (!nodeId) return '-'
		return nodeDetail.find(n => n.uuid === nodeId)?.name || nodeId
	}

	const parseInstanceRole = (instanceId: string) => {
		const id = instanceId || ''
		if (id.endsWith('-entry')) return { kind: 'entry' as const }
		const hopMatch = id.match(/-hop(\d+)(?:-relay(\d+))?$/)
		if (hopMatch) {
			return {
				kind: 'hop' as const,
				hopIndex: Number(hopMatch[1]),
				relayIndex: hopMatch[2] !== undefined ? Number(hopMatch[2]) : undefined
			}
		}
		const relayMatch = id.match(/-relay-(\d+)$/)
		if (relayMatch) return { kind: 'relay' as const, relayIndex: Number(relayMatch[1]) }
		return { kind: 'other' as const }
	}

	const instanceGroups = useMemo(() => {
		const entry: PlannedInstance[] = []
		const relays: PlannedInstance[] = []
		const hopsByIndex = new Map<number, PlannedInstance[]>()
		const others: PlannedInstance[] = []

		for (const ins of instances) {
			const role = parseInstanceRole(ins.instance_id)
			if (role.kind === 'entry') entry.push(ins)
			else if (role.kind === 'relay') relays.push(ins)
			else if (role.kind === 'hop') {
				const list = hopsByIndex.get(role.hopIndex) || []
				list.push(ins)
				hopsByIndex.set(role.hopIndex, list)
			} else {
				others.push(ins)
			}
		}

		entry.sort((a, b) => a.instance_id.localeCompare(b.instance_id))
		relays.sort((a, b) => a.instance_id.localeCompare(b.instance_id))
		const hops = Array.from(hopsByIndex.entries())
			.sort((a, b) => a[0] - b[0])
			.map(([hopIndex, list]) => ({
				hopIndex,
				instances: list.sort((a, b) => a.instance_id.localeCompare(b.instance_id))
			}))
		others.sort((a, b) => a.instance_id.localeCompare(b.instance_id))
		return { entry, relays, hops, others }
	}, [instances])

	const orderedInstances = useMemo(
		() => [...instanceGroups.entry, ...instanceGroups.relays, ...instanceGroups.hops.flatMap(h => h.instances), ...instanceGroups.others],
		[instanceGroups]
	)

	const instanceSummary = (instanceId: string) => {
		const st = statsByInstance[instanceId]
		const statsObj = st?.stats && typeof st.stats === 'object' ? st.stats : null
		return {
			currentConn: Number(statsObj?.current_connections ?? 0),
			inBytes: Number(statsObj?.total_inbound_bytes ?? 0),
			outBytes: Number(statsObj?.total_outbound_bytes ?? 0),
			last: st?.last_updated_at || ''
		}
	}

	const instanceRoute = (instanceId: string) => {
		const raw = routeOverride[instanceId] ?? statsByInstance[instanceId]?.route
		if (!raw || typeof raw !== 'object') return null
		return {
			preferred_backend: (raw as any).preferred_backend || '',
			last_success_backend: (raw as any).last_success_backend || ''
		}
	}

	const refreshRoute = async (instanceId: string) => {
		if (!instanceId) return
		try {
			const res = await fetch(`/api/v1/instances/${encodeURIComponent(instanceId)}/route`)
			if (!res.ok) throw new Error(`HTTP ${res.status}`)
			const body = await res.json()
			setRouteOverride(prev => ({ ...prev, [instanceId]: body.data || null }))
		} catch {
			// ignore
		}
	}

	const latencyMap = useMemo(() => parseLatencyMap(entryStat?.nodes_latency), [entryStat])

	const latencyData = useMemo(() => {
		if (topology?.type === 'relay_group' && topology?.relays?.length) {
			return topology.relays.map(r => ({
				name: r.name || r.node_id,
				latency: Number(latencyMap[r.node_id] || 0)
			}))
		}
		return stats.map(s => ({
			name: s.node_id,
			latency: parseLatency(s.nodes_latency)
		}))
	}, [latencyMap, stats, topology])

	const entryStatus = topology?.entry?.status || 'unknown'
	const statusColor = (status: string) => {
		if (status === 'healthy') return 'green'
		if (status === 'degraded') return 'yellow'
		if (status === 'faulty') return 'red'
		return 'gray'
	}

	const activeRelayInfo = useMemo(() => {
		if (!topology) return null
		if (topology.type === 'relay_group') {
			const activeId = topology.active_relay_node_id
			if (!activeId) return null
			const relay = topology.relays?.find(r => r.node_id === activeId)
			return {
				name: relay?.name || activeId,
				latency: Number(latencyMap[activeId] || 0)
			}
		}
		return null
	}, [latencyMap, topology])

	const ackAlert = async (alertId: number) => {
		if (!ruleId) return
		const res = await fetch(`/api/v1/forwards/${ruleId}/alert-history/${alertId}/acknowledge`, { method: 'POST' })
		if (res.ok) fetchAlerts()
	}

	return (
		<Flex direction="column" gap="4" className="p-4">
			<Flex justify="between" align="center">
				<Flex gap="2" align="center">
					<Button variant="ghost" onClick={() => navigate(-1)}>
						<ArrowLeftIcon /> {t('common.back', { defaultValue: '返回' })}
					</Button>
					<Text size="6" weight="bold">
						{t('forward.dashboard', { defaultValue: '转发监控面板' })}
					</Text>
				</Flex>
			</Flex>

			<Grid columns="2" gap="4">
				<Card>
					<Text weight="bold">{t('forward.topology', { defaultValue: '链路拓扑' })}</Text>
					<div className="mt-3 flex flex-wrap items-center gap-2">
						<Badge color="gray">客户端</Badge>
						<Text>→</Text>
						<Badge color={statusColor(entryStatus)}>{topology?.entry?.name || '-'}</Badge>
						{topology?.type === 'relay_group' && (
							<>
								<Text>→</Text>
								<Badge color="gray">{t('forward.relayGroup')}</Badge>
								{topology?.relays?.map(relay => (
									<Badge key={relay.node_id} color={relay.node_id === topology?.active_relay_node_id ? 'green' : 'gray'}>
										{relay.name || relay.node_id}
									</Badge>
								))}
							</>
						)}
						{topology?.type === 'chain' &&
							topology?.hops?.map((hop, idx) => (
								<Flex key={`${hop.type}-${idx}`} align="center" gap="2">
									<Text>→</Text>
									<Badge color="gray">{hop.type}</Badge>
									{hop.node && <Badge color={statusColor(hop.node.status || '')}>{hop.node.name}</Badge>}
									{hop.relays?.map(relay => (
										<Badge key={relay.node_id} color={relay.node_id === hop.active_relay_node_id ? 'green' : 'gray'}>
											{relay.name || relay.node_id}
										</Badge>
									))}
								</Flex>
							))}
						<Text>→</Text>
						<Badge color="gray">{topology?.target?.name || topology?.target?.ip || '-'}</Badge>
					</div>
					{activeRelayInfo && (
						<Text size="2" color="gray" className="mt-2 block">
							{t('forward.activeRelay', { defaultValue: '当前活动' })}: {activeRelayInfo.name}{' '}
							{activeRelayInfo.latency ? `(${activeRelayInfo.latency}ms)` : ''}
						</Text>
					)}
				</Card>

				<Card>
					<Text weight="bold">{t('forward.coreStatus', { defaultValue: '核心状态' })}</Text>
					<Flex gap="3" mt="3" align="center">
						<Badge color={statusColor(entryStatus)}>{entryStatus}</Badge>
						<Text>
							{t('chart.connections')}: {totals.connections}
						</Text>
						<Text>
							{t('common.traffic', { defaultValue: '流量' })}: {formatBytes(totals.in)} / {formatBytes(totals.out)}
						</Text>
					</Flex>
				</Card>
				</Grid>

				<Card>
					<Flex justify="between" align="center" wrap="wrap" gap="2">
						<Text weight="bold">{t('forward.instances', { defaultValue: '实例视图' })}</Text>
						<Button variant="ghost" onClick={fetchInstances} disabled={!ruleId}>
							{t('common.refresh', { defaultValue: '刷新' })}
						</Button>
					</Flex>
					<Text size="2" color="gray" className="mt-1 block">
						{t('forward.instancesHint', { defaultValue: '按 entry/relay/hop 展示实例，并用 preferred_backend 高亮当前链路。' })}
					</Text>

					<Table.Root className="mt-3">
						<Table.Header>
							<Table.Row>
								<Table.ColumnHeaderCell>{t('forward.role', { defaultValue: '角色' })}</Table.ColumnHeaderCell>
								<Table.ColumnHeaderCell>{t('forward.node', { defaultValue: '节点' })}</Table.ColumnHeaderCell>
								<Table.ColumnHeaderCell>{t('forward.listen', { defaultValue: '监听' })}</Table.ColumnHeaderCell>
								<Table.ColumnHeaderCell>{t('forward.backends', { defaultValue: '后端' })}</Table.ColumnHeaderCell>
								<Table.ColumnHeaderCell>{t('chart.connections')}</Table.ColumnHeaderCell>
								<Table.ColumnHeaderCell>{t('common.traffic', { defaultValue: '流量' })}</Table.ColumnHeaderCell>
								<Table.ColumnHeaderCell>{t('forward.updatedAt', { defaultValue: '更新时间' })}</Table.ColumnHeaderCell>
								<Table.ColumnHeaderCell>{t('forward.actions', { defaultValue: '操作' })}</Table.ColumnHeaderCell>
							</Table.Row>
						</Table.Header>
						<Table.Body>
							{orderedInstances.map(ins => {
								const role = parseInstanceRole(ins.instance_id)
								const label =
									role.kind === 'entry'
										? 'entry'
										: role.kind === 'relay'
											? `relay-${role.relayIndex ?? ''}`
											: role.kind === 'hop'
												? `hop${role.hopIndex}${role.relayIndex !== undefined ? `-relay${role.relayIndex}` : ''}`
												: 'other'
								const backends = [ins.remote, ...(ins.extra_remotes || [])].filter(Boolean)
								const route = instanceRoute(ins.instance_id)
								const preferred = route?.preferred_backend || ''
								const lastSuccess = route?.last_success_backend || ''
								const sum = instanceSummary(ins.instance_id)
								return (
									<Table.Row key={ins.instance_id}>
										<Table.Cell>
											<Flex direction="column" gap="1">
												<Text>{label}</Text>
												<Text size="1" color="gray">
													{ins.instance_id}
												</Text>
											</Flex>
										</Table.Cell>
										<Table.Cell>{nodeName(ins.node_id)}</Table.Cell>
										<Table.Cell>
											<Text>{ins.listen || `${ins.listen_port}`}</Text>
										</Table.Cell>
										<Table.Cell>
											<Flex wrap="wrap" gap="1" align="center">
												{backends.length === 0 ? (
													<Badge color="gray">-</Badge>
												) : (
													backends.map(b => (
														<Badge key={b} color={preferred && b === preferred ? 'green' : 'gray'}>
															{b}
														</Badge>
													))
												)}
												{lastSuccess && (
													<Text size="1" color="gray">
														{t('forward.lastSuccess', { defaultValue: '最近成功' })}: {lastSuccess}
													</Text>
												)}
											</Flex>
										</Table.Cell>
										<Table.Cell>{sum.currentConn}</Table.Cell>
										<Table.Cell>
											{formatBytes(sum.inBytes)} / {formatBytes(sum.outBytes)}
										</Table.Cell>
										<Table.Cell>
											<Text size="1" color="gray">
												{sum.last || '-'}
											</Text>
										</Table.Cell>
										<Table.Cell>
											<Flex gap="2" align="center">
												<Button size="1" variant="soft" onClick={() => setConnectionsInstance(ins.instance_id)}>
													{t('forward.connections', { defaultValue: '连接' })}
												</Button>
												<Button size="1" variant="ghost" onClick={() => refreshRoute(ins.instance_id)}>
													{t('forward.refreshRoute', { defaultValue: '刷新 route' })}
												</Button>
											</Flex>
										</Table.Cell>
									</Table.Row>
								)
							})}
						</Table.Body>
					</Table.Root>
				</Card>

				<Grid columns="2" gap="4">
					<Card>
						<Text weight="bold">{t('forward.realtimeTraffic', { defaultValue: '实时流量' })}</Text>
						<ChartContainer
						className="mt-2 h-[220px]"
						config={{
							in: { label: `${t('forward.trafficIn', { defaultValue: '入口' })} (Mbps)`, color: 'var(--chart-1)' },
							out: { label: `${t('forward.trafficOut', { defaultValue: '出口' })} (Mbps)`, color: 'var(--chart-2)' }
						}}>
						<LineChart data={trafficSeries}>
							<CartesianGrid strokeDasharray="3 3" />
							<XAxis dataKey="time" />
							<YAxis />
							<ChartTooltip content={<ChartTooltipContent />} />
							<Line type="monotone" dataKey="in" stroke="var(--color-chart-1)" dot={false} />
							<Line type="monotone" dataKey="out" stroke="var(--color-chart-2)" dot={false} />
						</LineChart>
					</ChartContainer>
				</Card>
				<Card>
					<Text weight="bold">{t('forward.latency', { defaultValue: '节点延迟' })}</Text>
					<ChartContainer
						className="mt-2 h-[220px]"
						config={{
							latency: { label: t('forward.latency', { defaultValue: '延迟' }), color: 'var(--chart-3)' }
						}}>
						<BarChart data={latencyData}>
							<CartesianGrid strokeDasharray="3 3" />
							<XAxis dataKey="name" />
							<YAxis />
							<Tooltip />
							<Bar dataKey="latency" fill="var(--color-chart-3)" />
						</BarChart>
					</ChartContainer>
				</Card>
			</Grid>

			<Grid columns="2" gap="4">
				<Card>
					<Text weight="bold">{t('forward.nodeHealth', { defaultValue: '节点健康状态' })}</Text>
					<div className="mt-3 space-y-2">
						{stats.map(stat => (
							<Flex key={stat.node_id} justify="between" align="center" className="border-b border-(--gray-4) pb-2">
								<Text>{stat.node_id}</Text>
								<Badge color={statusColor(stat.link_status)}>{stat.link_status}</Badge>
							</Flex>
						))}
					</div>
				</Card>
				<Card>
					<Text weight="bold">{t('forward.alertHistory', { defaultValue: '告警历史' })}</Text>
					<div className="mt-3 space-y-2">
						{alerts.length === 0 ? (
							<Text size="2" color="gray">
								{t('forward.noAlert', { defaultValue: '暂无告警' })}
							</Text>
						) : (
							alerts.map(item => (
								<Flex key={item.id} justify="between" align="center" className="border-b border-(--gray-4) pb-2">
									<div>
										<Text>{item.message}</Text>
										<Text size="1" color="gray">
											{item.created_at}
										</Text>
									</div>
									{item.acknowledged ? (
										<Badge color="green">{t('forward.acknowledged', { defaultValue: '已确认' })}</Badge>
									) : (
										<Button size="1" variant="soft" onClick={() => ackAlert(item.id)}>
											{t('forward.acknowledge', { defaultValue: '确认' })}
										</Button>
									)}
								</Flex>
							))
						)}
					</div>
				</Card>
				</Grid>

				<InstanceConnectionsDialog
					open={!!connectionsInstance}
					instanceId={connectionsInstance || undefined}
					onClose={() => setConnectionsInstance(null)}
				/>
			</Flex>
		)
	}

function parseLatency(raw?: string) {
	if (!raw) return 0
	try {
		const data = JSON.parse(raw)
		if (data.self !== undefined) return data.self
		const values = Object.values(data)
		return values.length ? Number(values[0]) : 0
	} catch {
		return 0
	}
}

function parseLatencyMap(raw?: string): Record<string, number> {
	if (!raw) return {}
	try {
		const data = JSON.parse(raw)
		if (!data || typeof data !== 'object') return {}
		const out: Record<string, number> = {}
		for (const [k, v] of Object.entries(data)) {
			out[String(k)] = Number(v)
		}
		return out
	} catch {
		return {}
	}
}

const ForwardDashboard = () => (
	<NodeDetailsProvider>
		<ForwardDashboardInner />
	</NodeDetailsProvider>
)

export default ForwardDashboard
