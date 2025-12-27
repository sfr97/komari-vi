import { useEffect, useMemo, useState } from 'react'
import { Button, Dialog, Flex, Select, Table, Text } from '@radix-ui/themes'
import { ReloadIcon } from '@radix-ui/react-icons'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

type ConnectionRow = {
	src_ip: string
	src_port: number
	duration_secs: number
	backend: string
}

type ConnectionsResponse =
	| {
			id: string
			protocol: 'tcp'
			total: number
			limit: number
			offset: number
			connections: ConnectionRow[]
	  }
	| {
			id: string
			protocol: 'udp'
			total: number
			limit: number
			offset: number
			sessions: ConnectionRow[]
	  }
	| {
			id: string
			protocol: 'all'
			tcp_total: number
			udp_total: number
			limit: number
			offset: number
			connections: ConnectionRow[]
			sessions: ConnectionRow[]
	  }

type Props = {
	open: boolean
	instanceId?: string
	onClose: () => void
}

const LIMIT = 100

const InstanceConnectionsDialog = ({ open, instanceId, onClose }: Props) => {
	const { t } = useTranslation()
	const [loading, setLoading] = useState(false)
	const [protocol, setProtocol] = useState<'all' | 'tcp' | 'udp'>('all')
	const [page, setPage] = useState(0)
	const [data, setData] = useState<ConnectionsResponse | null>(null)

	const offset = page * LIMIT

	const totalForPaging = useMemo(() => {
		if (!data) return 0
		if (data.protocol === 'tcp' || data.protocol === 'udp') return Number(data.total || 0)
		return Math.max(Number(data.tcp_total || 0), Number(data.udp_total || 0))
	}, [data])

	const totalPages = Math.max(1, Math.ceil(totalForPaging / LIMIT))

	const load = async () => {
		if (!instanceId) return
		setLoading(true)
		try {
			const query = new URLSearchParams()
			query.set('limit', String(LIMIT))
			query.set('offset', String(offset))
			if (protocol !== 'all') query.set('protocol', protocol)
			const res = await fetch(`/api/v1/instances/${encodeURIComponent(instanceId)}/connections?${query.toString()}`)
			if (!res.ok) throw new Error(`HTTP ${res.status}`)
			const body = await res.json()
			setData(body.data || null)
		} catch (e: any) {
			toast.error(e?.message || 'Load connections failed')
			setData(null)
		} finally {
			setLoading(false)
		}
	}

	useEffect(() => {
		if (!open) return
		setPage(0)
		setData(null)
	}, [open, instanceId])

	useEffect(() => {
		if (!open || !instanceId) return
		load()
		// eslint-disable-next-line react-hooks/exhaustive-deps
	}, [open, instanceId, protocol, page])

	const rows = useMemo(() => {
		if (!data) return [] as Array<{ proto: 'tcp' | 'udp'; row: ConnectionRow }>
		if (data.protocol === 'tcp') return (data.connections || []).map(r => ({ proto: 'tcp' as const, row: r }))
		if (data.protocol === 'udp') return (data.sessions || []).map(r => ({ proto: 'udp' as const, row: r }))
		const merged: Array<{ proto: 'tcp' | 'udp'; row: ConnectionRow }> = []
		for (const r of data.connections || []) merged.push({ proto: 'tcp', row: r })
		for (const r of data.sessions || []) merged.push({ proto: 'udp', row: r })
		return merged
	}, [data])

	const headerRight = (
		<Flex gap="2" align="center">
			<Select.Root value={protocol} onValueChange={v => setProtocol(v as any)} disabled={!instanceId}>
				<Select.Trigger placeholder="protocol" />
				<Select.Content>
					<Select.Item value="all">{t('forward.protocolAll', { defaultValue: '全部' })}</Select.Item>
					<Select.Item value="tcp">TCP</Select.Item>
					<Select.Item value="udp">UDP</Select.Item>
				</Select.Content>
			</Select.Root>
			<Button variant="ghost" onClick={load} disabled={loading || !instanceId}>
				<ReloadIcon /> {t('common.refresh', { defaultValue: '刷新' })}
			</Button>
		</Flex>
	)

	return (
		<Dialog.Root open={open} onOpenChange={o => (!o ? onClose() : null)}>
			<Dialog.Content maxWidth="900px">
				<Dialog.Title>{t('forward.instanceConnections', { defaultValue: '连接列表' })}</Dialog.Title>
				<Flex justify="between" align="center" mb="3" wrap="wrap" gap="2">
					<Text size="2" color="gray">
						{instanceId || '-'}
					</Text>
					{headerRight}
				</Flex>

				{data?.protocol === 'all' && (
					<Text size="1" color="gray" className="mb-2 block">
						TCP total: {data.tcp_total || 0} · UDP total: {data.udp_total || 0} · limit: {data.limit || LIMIT} · offset: {data.offset || 0}
					</Text>
				)}
				{(data?.protocol === 'tcp' || data?.protocol === 'udp') && (
					<Text size="1" color="gray" className="mb-2 block">
						total: {(data as any)?.total ?? 0} · limit: {data?.limit || LIMIT} · offset: {data?.offset || 0}
					</Text>
				)}

				{rows.length === 0 ? (
					<Text size="2" color="gray">
						{loading ? t('common.loading') : t('forward.noConnections', { defaultValue: '暂无连接' })}
					</Text>
				) : (
					<Table.Root>
						<Table.Header>
							<Table.Row>
								<Table.ColumnHeaderCell>{t('forward.protocol', { defaultValue: '协议' })}</Table.ColumnHeaderCell>
								<Table.ColumnHeaderCell>{t('forward.src', { defaultValue: '来源' })}</Table.ColumnHeaderCell>
								<Table.ColumnHeaderCell>{t('forward.duration', { defaultValue: '时长(s)' })}</Table.ColumnHeaderCell>
								<Table.ColumnHeaderCell>{t('forward.backend', { defaultValue: '后端' })}</Table.ColumnHeaderCell>
							</Table.Row>
						</Table.Header>
						<Table.Body>
							{rows.map((item, idx) => (
								<Table.Row key={`${item.proto}-${item.row.src_ip}:${item.row.src_port}-${item.row.backend}-${idx}`}>
									<Table.Cell>{item.proto.toUpperCase()}</Table.Cell>
									<Table.Cell>
										{item.row.src_ip}:{item.row.src_port}
									</Table.Cell>
									<Table.Cell>{item.row.duration_secs}</Table.Cell>
									<Table.Cell>{item.row.backend}</Table.Cell>
								</Table.Row>
							))}
						</Table.Body>
					</Table.Root>
				)}

				<Flex justify="between" align="center" mt="3" wrap="wrap" gap="2">
					<Text size="1" color="gray">
						{t('forward.page', { defaultValue: '页' })}: {page + 1} / {totalPages}
					</Text>
					<Flex gap="2" align="center">
						<Button variant="soft" disabled={page <= 0 || loading} onClick={() => setPage(p => Math.max(0, p - 1))}>
							{t('common.prev', { defaultValue: '上一页' })}
						</Button>
						<Button
							variant="soft"
							disabled={loading || page + 1 >= totalPages}
							onClick={() => setPage(p => (p + 1 >= totalPages ? p : p + 1))}
						>
							{t('common.next', { defaultValue: '下一页' })}
						</Button>
						<Button variant="ghost" onClick={onClose}>
							{t('common.close')}
						</Button>
					</Flex>
				</Flex>
			</Dialog.Content>
		</Dialog.Root>
	)
}

export default InstanceConnectionsDialog

