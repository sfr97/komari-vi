import { Checkbox } from '@/components/ui/checkbox'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { NodeDetailsProvider, useNodeDetails } from '@/contexts/NodeDetailsContext'
import { OfflineNotificationProvider, useOfflineNotification, type OfflineNotification } from '@/contexts/NotificationContext'
import React from 'react'
import { BarChart3, Pencil, ScrollText, Search } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { Badge, Button, Dialog, Flex, IconButton, Switch, TextField, Tooltip } from '@radix-ui/themes'
import { toast } from 'sonner'
import Loading from '@/components/loading'
import Tips from '@/components/ui/tips'
import Flag from '@/components/Flag'
import ServerViewModeControl, { type ServerViewMode } from '@/components/server/ServerViewModeControl'
import ServerGroupedTableBody from '@/components/server/ServerGroupedTableBody'
import { Bar, BarChart, CartesianGrid, XAxis, YAxis } from 'recharts'
import { ChartContainer, ChartTooltip, ChartTooltipContent } from '@/components/ui/chart'

type AgentConnectionLog = {
	id: number
	client: string
	connection_id: number
	connected_at: string
	disconnected_at?: string | null
	online_seconds?: number | null
}

function formatDurationSeconds(totalSeconds: number, t: any) {
	const s = Math.max(0, Math.floor(totalSeconds))
	const days = Math.floor(s / 86400)
	const hours = Math.floor((s % 86400) / 3600)
	const minutes = Math.floor((s % 3600) / 60)
	const seconds = s % 60
	if (days > 0) return `${days}${t('nodeCard.time_day')}${hours}${t('nodeCard.time_hour')}${minutes}${t('nodeCard.time_minute')}${seconds}${t('nodeCard.time_second')}`
	if (hours > 0) return `${hours}${t('nodeCard.time_hour')}${minutes}${t('nodeCard.time_minute')}${seconds}${t('nodeCard.time_second')}`
	if (minutes > 0) return `${minutes}${t('nodeCard.time_minute')}${seconds}${t('nodeCard.time_second')}`
	return `${seconds}${t('nodeCard.time_second')}`
}

const OfflinePage = () => {
	return (
		<OfflineNotificationProvider>
			<NodeDetailsProvider>
				<InnerLayout />
			</NodeDetailsProvider>
		</OfflineNotificationProvider>
	)
}
const NotificationEditForm = ({
	initialValues,
	onSubmit,
	loading,
	onCancel
}: {
	initialValues: { enable: boolean; cooldown: number; grace_period: number }
	onSubmit: (values: { enable: boolean; cooldown: number; grace_period: number }) => void
	loading?: boolean
	onCancel?: () => void
}) => {
	const { t } = useTranslation()
	const [enabled, setEnabled] = React.useState(initialValues.enable)
	// const [cooldown, setCooldown] = React.useState(initialValues.cooldown);
	const [grace, setGrace] = React.useState(initialValues.grace_period)
	return (
		<form
			onSubmit={e => {
				e.preventDefault()
				onSubmit({ enable: enabled, cooldown: 3000, grace_period: grace })
			}}
			className="flex flex-col gap-2">
			<label htmlFor="status">{t('common.status')}</label>
			<Switch id="status" name="status" checked={enabled} onCheckedChange={setEnabled} />
			{/* <label htmlFor="cooldown">{t("notification.offline.cooldown")}</label>
      <TextField.Root
        type="number"
        min={0}
        value={cooldown}
        onChange={e => setCooldown(Number(e.target.value))}
        id="cooldown"
        name="cooldown"
      /> */}
			<label htmlFor="grace_period" className="flex items-center gap-2">
				{t('notification.offline.grace_period')}
				<Tips>{t('notification.offline.grace_period_tip')}</Tips>
			</label>
			<TextField.Root type="number" min={0} value={grace} onChange={e => setGrace(Number(e.target.value))} id="grace_period" name="grace_period" />
			<Flex gap="2" justify="end" className="mt-4">
				{onCancel && (
					<Dialog.Close>
						<Button variant="soft" color="gray" type="button" onClick={onCancel}>
							{t('common.cancel')}
						</Button>
					</Dialog.Close>
				)}
				<Button variant="solid" type="submit" disabled={loading}>
					{t('common.save')}
				</Button>
			</Flex>
		</form>
	)
}

const InnerLayout = () => {
	const [search, setSearch] = React.useState('')
	const [selected, setSelected] = React.useState<string[]>([])
	const { loading: onLoading, error: onError, offlineNotification, refresh } = useOfflineNotification()
	const { isLoading: onNodeLoading, error: onNodeError } = useNodeDetails()
	const { t } = useTranslation()
	const [batchLoading, setBatchLoading] = React.useState(false)
	const [batchDialogOpen, setBatchDialogOpen] = React.useState(false)
	const [batchForm, setBatchForm] = React.useState({
		enable: true,
		cooldown: 1800,
		grace_period: 300
	})

	// 批量修改
	const handleBatchEdit = (values: { enable: boolean; cooldown: number; grace_period: number }) => {
		setBatchLoading(true)
		const payload = selected.map(id => ({
			client: id,
			enable: values.enable,
			cooldown: values.cooldown,
			grace_period: values.grace_period
		}))
		fetch('/api/admin/notification/offline/edit', {
			method: 'POST',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify(payload)
		})
			.then(res => {
				if (!res.ok) {
					toast.error('Failed to update offline notifications: ' + res.statusText)
				} else {
					toast.success(t('common.updated_successfully'))
				}
				return res.json()
			})
			.then(() => {
				setBatchLoading(false)
				setBatchDialogOpen(false)
				refresh()
			})
			.catch(error => {
				console.error('Error updating offline notifications:', error)
				toast.error(t('common.error', { message: error.message }))
				setBatchLoading(false)
			})
	}

	if (onLoading || onNodeLoading) {
		return <Loading text="(o゜▽゜)o☆" />
	}
	if (onError || onNodeError) {
		return <div>Error: {onError?.message || onNodeError}</div>
	}
	return (
		<div className="flex flex-col gap-4 md:p-4 p-1">
			<Flex justify="between" align="center" wrap="wrap">
				<label className="text-2xl font-semibold">{t('notification.offline.full_title', '离线通知设置')}</label>
				<TextField.Root
					type="text"
					className="max-w-64"
					placeholder={t('common.search')}
					value={search}
					onChange={(e: React.ChangeEvent<HTMLInputElement>) => setSearch(e.target.value)}>
					<TextField.Slot>
						<Search size={16} />
					</TextField.Slot>
				</TextField.Root>
			</Flex>
			<OfflineNotificationTable search={search} selected={selected} onSelectionChange={setSelected} />
			<label className="text-sm text-muted-foreground">
				{t('common.selected', {
					count: selected.length
				})}
			</label>
			<Flex gap="2" align="center">
				<Dialog.Root open={batchDialogOpen} onOpenChange={setBatchDialogOpen}>
					<Dialog.Trigger>
						<Button
							variant="soft"
							onClick={() => {
								// 默认取第一个选中项的配置作为初始值
								const first = offlineNotification.find(n => n.client === selected[0])
								setBatchForm({
									enable: first?.enable ?? true,
									cooldown: first?.cooldown ?? 1800,
									grace_period: first?.grace_period ?? 300
								})
							}}
							disabled={batchLoading || selected.length === 0}>
							{t('notification.offline.batch_edit')}
						</Button>
					</Dialog.Trigger>
					<Dialog.Content>
						<Dialog.Title>{t('notification.offline.batch_edit')}</Dialog.Title>
						<NotificationEditForm
							initialValues={batchForm}
							loading={batchLoading}
							onSubmit={handleBatchEdit}
							onCancel={() => setBatchDialogOpen(false)}
						/>
					</Dialog.Content>
				</Dialog.Root>
			</Flex>
			<label className="text-sm text-muted-foreground">
				<span dangerouslySetInnerHTML={{ __html: t('notification.offline.tips') }} />
			</label>
		</div>
	)
}

const OfflineNotificationTable = ({
	search,
	selected,
	onSelectionChange
}: {
	search: string
	selected: string[]
	onSelectionChange: (ids: string[]) => void
}) => {
	const { offlineNotification } = useOfflineNotification()
	const { nodeDetail } = useNodeDetails()
	const { t } = useTranslation()
	const [viewMode, setViewMode] = React.useState<ServerViewMode>('list')

	const filtered = [...nodeDetail].sort((a, b) => a.weight - b.weight).filter(node => node.name.toLowerCase().includes(search.toLowerCase()))
	return (
		<div className="overflow-hidden">
			<Flex justify="end" className="mb-3">
				<ServerViewModeControl value={viewMode} onValueChange={setViewMode} size="1" />
			</Flex>
			<Table>
				<TableHeader>
					<TableRow>
						<TableHead className="w-6">
							<Checkbox
								checked={selected.length === filtered.length ? true : selected.length > 0 ? 'indeterminate' : false}
								onCheckedChange={checked => onSelectionChange(checked ? filtered.map(n => n.uuid) : [])}
							/>
						</TableHead>
						<TableHead>{t('common.server')}</TableHead>
						<TableHead>{t('common.status')}</TableHead>
						{/* <TableHead>{t("notification.offline.cooldown")}</TableHead> */}
						<TableHead>{t('notification.offline.grace_period')}</TableHead>
						<TableHead>{t('notification.offline.last_notified')}</TableHead>
						<TableHead>{t('common.action')}</TableHead>
					</TableRow>
				</TableHeader>
				<TableBody>
					<ServerGroupedTableBody
						items={filtered}
						viewMode={viewMode}
						colSpan={6}
						ungroupedLabel={t('common.ungrouped', { defaultValue: '未分组' })}
						unknownRegionLabel={t('common.unknown_region', { defaultValue: '未知地域' })}
						renderRow={node => (
							<TableRow key={node.uuid}>
								<TableCell>
									<Checkbox
										checked={selected.includes(node.uuid)}
										onCheckedChange={checked => {
											if (checked) {
												onSelectionChange([...selected, node.uuid])
											} else {
												onSelectionChange(selected.filter(id => id !== node.uuid))
											}
										}}
									/>
								</TableCell>
								<TableCell>
									<div className="flex items-center gap-2">
										<Flag flag={node.region ?? ''} size="4" />
										<span className="truncate">{node.name}</span>
									</div>
								</TableCell>
								<TableCell>
									<Badge color={offlineNotification.find(n => n.client === node.uuid)?.enable ? 'green' : 'red'}>
										{offlineNotification.find(n => n.client === node.uuid)?.enable ? t('common.enabled') : t('common.disabled')}
									</Badge>
								</TableCell>
								<TableCell>
									{offlineNotification.find(n => n.client === node.uuid)?.grace_period || 300}
									{t('nodeCard.time_second')}
								</TableCell>
								<TableCell>
									{(() => {
										const lastNotified = offlineNotification.find(n => n.client === node.uuid)?.last_notified
										if (!lastNotified) return '-'
										const date = new Date(lastNotified)
										if (date.getFullYear() < 3) return t('notification.offline.never_triggered')
										return date.toLocaleString()
									})()}
								</TableCell>
								<TableCell>
									<ActionButtons clientUUID={node.uuid} offlineNotifications={offlineNotification.find(n => n.client === node.uuid)} />
								</TableCell>
							</TableRow>
						)}
					/>
				</TableBody>
			</Table>
		</div>
	)
}

const ConnectionStatsPanel = ({ logs, loading, now }: { logs: AgentConnectionLog[]; loading: boolean; now: number }) => {
	const { t } = useTranslation()
	if (loading) {
		return <div className="p-3 text-sm text-muted-foreground">{t('common.loading', { defaultValue: '加载中...' })}</div>
	}
	if (!logs.length) {
		return <div className="p-3 text-sm text-muted-foreground">{t('common.no_data', { defaultValue: '暂无数据' })}</div>
	}

	const asc = [...logs].sort((a, b) => new Date(a.connected_at).getTime() - new Date(b.connected_at).getTime())
	let prevDisconnected: number | null = null
	const series = asc.map((l, idx) => {
		const connectedAt = new Date(l.connected_at).getTime()
		const disconnectedAt = l.disconnected_at ? new Date(l.disconnected_at).getTime() : now
		const onlineSec = l.online_seconds ?? Math.max(0, Math.floor((disconnectedAt - connectedAt) / 1000))
		const offlineGapSec = prevDisconnected ? Math.max(0, Math.floor((connectedAt - prevDisconnected) / 1000)) : 0
		prevDisconnected = l.disconnected_at ? disconnectedAt : now
		return {
			idx: idx + 1,
			online: onlineSec,
			offline: offlineGapSec
		}
	})

	return (
		<div className="w-[560px] max-w-[80vw] p-3">
			<div className="mb-2 text-xs text-muted-foreground">
				{t('notification.offline.stats_hint', { defaultValue: '展示最近会话的在线/离线间隔（最多 3000 条）' })}
			</div>
			<ChartContainer
				className="h-[220px]"
				config={{
					online: { label: t('notification.offline.stats_online', { defaultValue: '在线时长(秒)' }), color: 'var(--chart-2)' },
					offline: { label: t('notification.offline.stats_offline', { defaultValue: '离线空窗(秒)' }), color: 'var(--chart-1)' }
				}}>
				<BarChart data={series}>
					<CartesianGrid strokeDasharray="3 3" />
					<XAxis dataKey="idx" tick={false} />
					<YAxis />
					<ChartTooltip content={<ChartTooltipContent />} />
					<Bar dataKey="offline" stackId="a" fill="var(--color-offline)" />
					<Bar dataKey="online" stackId="a" fill="var(--color-online)" />
				</BarChart>
			</ChartContainer>
		</div>
	)
}

const ActionButtons = ({ clientUUID, offlineNotifications }: { clientUUID: string; offlineNotifications: OfflineNotification | undefined }) => {
	const { t } = useTranslation()
	const { refresh } = useOfflineNotification()
	const [editOpen, setEditOpen] = React.useState(false)
	const [editSaving, setEditSaving] = React.useState(false)
	const [logsOpen, setLogsOpen] = React.useState(false)
	const [logsLoading, setLogsLoading] = React.useState(false)
	const [logs, setLogs] = React.useState<AgentConnectionLog[]>([])
	const [logsTotal, setLogsTotal] = React.useState(0)
	const [logsPage, setLogsPage] = React.useState(1)
	const pageSize = 50
	const [now, setNow] = React.useState(Date.now())

	const [statsLoading, setStatsLoading] = React.useState(false)
	const [statsLogs, setStatsLogs] = React.useState<AgentConnectionLog[]>([])

	React.useEffect(() => {
		if (!logsOpen) return
		const id = window.setInterval(() => setNow(Date.now()), 1000)
		return () => window.clearInterval(id)
	}, [logsOpen])

	const fetchLogs = React.useCallback(
		async (page: number) => {
			setLogsLoading(true)
			try {
				const res = await fetch(`/api/admin/notification/offline/logs?client=${encodeURIComponent(clientUUID)}&page=${page}&page_size=${pageSize}`)
				const json = await res.json()
				if (!res.ok || json.status !== 'success') throw new Error(json.message || res.statusText)
				setLogs(json.data?.logs || [])
				setLogsTotal(json.data?.total || 0)
				setLogsPage(json.data?.page || page)
			} catch (e: any) {
				toast.error(t('common.error', { message: e?.message || String(e) }))
			} finally {
				setLogsLoading(false)
			}
		},
		[clientUUID, pageSize, t]
	)

	const prefetchStats = React.useCallback(async () => {
		if (statsLoading || statsLogs.length > 0) return
		setStatsLoading(true)
		try {
			const res = await fetch(`/api/admin/notification/offline/logs/chart?client=${encodeURIComponent(clientUUID)}&limit=3000`)
			const json = await res.json()
			if (!res.ok || json.status !== 'success') throw new Error(json.message || res.statusText)
			setStatsLogs(json.data?.logs || [])
		} catch (e: any) {
			toast.error(t('common.error', { message: e?.message || String(e) }))
		} finally {
			setStatsLoading(false)
		}
	}, [clientUUID, statsLoading, statsLogs.length, t])

	React.useEffect(() => {
		if (!logsOpen) return
		setLogsPage(1)
		void fetchLogs(1)
	}, [logsOpen, fetchLogs])

	const pageCount = Math.max(1, Math.ceil(logsTotal / pageSize))

	return (
		<Flex gap="2" align="center">
			<Dialog.Root open={editOpen} onOpenChange={setEditOpen}>
				<Dialog.Trigger>
					<IconButton variant="ghost">
						<Pencil size={16} />
					</IconButton>
				</Dialog.Trigger>
				<Dialog.Content>
					<Dialog.Title>{t('common.edit')}</Dialog.Title>
					<NotificationEditForm
						initialValues={{
							enable: offlineNotifications?.enable ?? false,
							cooldown: offlineNotifications?.cooldown ?? 1800,
							grace_period: offlineNotifications?.grace_period ?? 300
						}}
						loading={editSaving}
						onSubmit={values => {
							setEditSaving(true)
							fetch('/api/admin/notification/offline/edit', {
								method: 'POST',
								headers: { 'Content-Type': 'application/json' },
								body: JSON.stringify([
									{
										client: clientUUID,
										...values
									}
								])
							})
								.then(res => {
									if (!res.ok) {
										toast.error('Failed to save offline notification settings: ' + res.statusText)
									}
									toast.success(t('common.updated_successfully'))
									return res.json()
								})
								.then(() => {
									setEditOpen(false)
									refresh()
									setEditSaving(false)
								})
								.catch(error => {
									console.error('Error saving offline notification settings:', error)
									toast.error(t('common.error', { message: error.message }))
								})
						}}
						onCancel={() => setEditOpen(false)}
					/>
				</Dialog.Content>
			</Dialog.Root>
			<Dialog.Root open={logsOpen} onOpenChange={setLogsOpen}>
				<Tooltip content={t('notification.offline.view_logs', { defaultValue: '查看连接日志' })}>
					<Dialog.Trigger>
						<IconButton variant="ghost">
							<ScrollText size={16} />
						</IconButton>
					</Dialog.Trigger>
				</Tooltip>
				<Dialog.Content style={{ maxWidth: 720 }}>
					<Dialog.Title>{t('notification.offline.connection_logs', { defaultValue: '连接日志' })}</Dialog.Title>
					<div className="text-xs text-muted-foreground">
						{t('notification.offline.connection_logs_hint', { defaultValue: '按时间倒序展示。仍在线的会话将实时显示在线时长。' })}
					</div>
					<div className="mt-3 max-h-[60vh] overflow-y-auto pr-2">
						{logsLoading ? (
							<div className="py-6 text-center text-sm text-muted-foreground">{t('common.loading', { defaultValue: '加载中...' })}</div>
						) : logs.length === 0 ? (
							<div className="py-6 text-center text-sm text-muted-foreground">{t('common.no_data', { defaultValue: '暂无数据' })}</div>
						) : (
							<div className="relative pl-4">
								<div className="absolute left-1 top-0 h-full w-px bg-(--gray-6)" />
								{logs.map(l => {
									const connectedAt = new Date(l.connected_at).getTime()
									const disconnectedAt = l.disconnected_at ? new Date(l.disconnected_at).getTime() : null
									const onlineSec = disconnectedAt
										? l.online_seconds ?? Math.max(0, Math.floor((disconnectedAt - connectedAt) / 1000))
										: Math.max(0, Math.floor((now - connectedAt) / 1000))
									return (
										<div key={l.id} className="relative mb-4">
											<div className={`absolute -left-[11px] top-1 h-2.5 w-2.5 rounded-full ${disconnectedAt ? 'bg-(--gray-9)' : 'bg-(--green-9)'}`} />
											<div className="text-sm">
												<span className="font-medium">{new Date(l.connected_at).toLocaleString()}</span>
												<span className="mx-2 text-muted-foreground">→</span>
												{disconnectedAt ? (
													<span className="font-medium">{new Date(l.disconnected_at as string).toLocaleString()}</span>
												) : (
													<span className="font-medium text-(--green-11)">{t('common.online', { defaultValue: '在线中' })}</span>
												)}
											</div>
											<div className="mt-1 flex flex-wrap items-center gap-3 text-xs text-muted-foreground">
												<span>
													{t('notification.offline.online_duration', { defaultValue: '在线时长' })}: {formatDurationSeconds(onlineSec, t)}
												</span>
												<span>
													{t('notification.offline.connection_id', { defaultValue: '连接ID' })}: {l.connection_id}
												</span>
											</div>
										</div>
									)
								})}
							</div>
						)}
					</div>
					<Flex justify="between" align="center" className="mt-4">
						<div className="text-xs text-muted-foreground">
							{t('common.total', { defaultValue: '总计' })}: {logsTotal}
						</div>
						<Flex gap="2" align="center">
							<Button variant="soft" disabled={logsLoading || logsPage <= 1} onClick={() => void fetchLogs(logsPage - 1)}>
								{t('common.prev', { defaultValue: '上一页' })}
							</Button>
							<div className="text-xs text-muted-foreground">
								{logsPage} / {pageCount}
							</div>
							<Button variant="soft" disabled={logsLoading || logsPage >= pageCount} onClick={() => void fetchLogs(logsPage + 1)}>
								{t('common.next', { defaultValue: '下一页' })}
							</Button>
						</Flex>
					</Flex>
				</Dialog.Content>
			</Dialog.Root>
			<Tooltip content={<ConnectionStatsPanel logs={statsLogs} loading={statsLoading} now={Date.now()} />}>
				<IconButton variant="ghost" onMouseEnter={() => void prefetchStats()}>
					<BarChart3 size={16} />
				</IconButton>
			</Tooltip>
		</Flex>
	)
}

export default OfflinePage
