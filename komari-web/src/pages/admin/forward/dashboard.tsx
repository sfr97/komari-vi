import { useEffect, useMemo, useState } from 'react'
import { useParams, useNavigate } from 'react-router-dom'
import { Badge, Button, Card, Flex, Grid, Text } from '@radix-ui/themes'
import { useTranslation } from 'react-i18next'
import { ArrowLeftIcon } from '@radix-ui/react-icons'
import { LineChart, Line, XAxis, YAxis, CartesianGrid, BarChart, Bar, Tooltip } from 'recharts'
import { ChartContainer, ChartTooltip, ChartTooltipContent } from '@/components/ui/chart'

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

const ForwardDashboard = () => {
	const { t } = useTranslation()
	const navigate = useNavigate()
	const { id } = useParams()
	const ruleId = Number(id || 0)
	const [stats, setStats] = useState<ForwardStat[]>([])
	const [history, setHistory] = useState<ForwardHistory[]>([])
	const [topology, setTopology] = useState<Topology | null>(null)
	const [totals, setTotals] = useState({ connections: 0, in: 0, out: 0 })
	const [alerts, setAlerts] = useState<AlertHistory[]>([])
	const [entryStat, setEntryStat] = useState<ForwardStat | null>(null)
	const [trafficSeries, setTrafficSeries] = useState<{ time: string; in: number; out: number }[]>([])

	const fetchStats = async () => {
		if (!ruleId) return
		const res = await fetch(`/api/v1/forwards/${ruleId}/stats`)
		if (!res.ok) throw new Error(`HTTP ${res.status}`)
		const body = await res.json()
		setStats(body.data?.stats || [])
		setHistory(body.data?.history || [])
		setEntryStat(body.data?.entry_status || null)
		setTotals({
			connections: body.data?.total_connections || 0,
			in: body.data?.total_traffic_in || 0,
			out: body.data?.total_traffic_out || 0
		})
		const bpsIn = Number(body.data?.entry_status?.realtime_bps_in || 0)
		const bpsOut = Number(body.data?.entry_status?.realtime_bps_out || 0)
		const point = { time: new Date().toLocaleTimeString(), in: bpsIn / 1_000_000, out: bpsOut / 1_000_000 }
		setTrafficSeries(prev => {
			const next = [...prev, point]
			return next.length > 60 ? next.slice(next.length - 60) : next
		})
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

	useEffect(() => {
		if (!ruleId) return
		const load = async () => {
			try {
				await Promise.all([fetchStats(), fetchTopology(), fetchAlerts()])
			} catch {
				// ignore
			}
		}
		load()
		const timer = setInterval(() => {
			fetchStats()
			fetchTopology()
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

export default ForwardDashboard
