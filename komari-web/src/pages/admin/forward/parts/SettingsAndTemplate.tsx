import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Badge, Button, Card, Flex, Grid, Select, Table, Text, TextField, TextArea } from '@radix-ui/themes'
import { toast } from 'sonner'

// --- SettingsPanel part ---
type Settings = {
	stats_report_interval: number
	health_check_interval: number
	history_aggregate_period: string
	realm_crash_restart_limit: number
	process_stop_timeout: number
}

const NumberField = ({
	label,
	help,
	value,
	min,
	max,
	onChange
}: {
	label: string
	help: string
	value: number
	min: number
	max: number
	onChange: (v: number) => void
}) => (
	<div className="flex flex-col gap-2">
		<Text size="2">{label}</Text>
		<Text size="1" color="gray">
			{help}
		</Text>
		<TextField.Root type="number" value={value} min={min} max={max} onChange={e => onChange(Number(e.target.value))} />
	</div>
)

// --- RealmBinaryManager part ---
type RealmBinary = {
	id: number
	os: string
	arch: string
	version: string
	file_path: string
	file_size: number
	file_hash: string
	is_default: boolean
	uploaded_at: string
}

const osOptions = ['linux']
const archOptions = [
	'x86_64',
	'aarch64',
	'armv7',
	'arm',
	'mips',
	'mipsle',
	'mips64',
	'mips64le',
	'armv7',
	'i686'
]

const SettingsAndTemplate = () => {
	const { t } = useTranslation()

	// --- State from SettingsPanel ---
	const [settingsLoading, setSettingsLoading] = useState(false)
	const [settings, setSettings] = useState<Settings>({
		stats_report_interval: 10,
		health_check_interval: 10,
		history_aggregate_period: '1hour',
		realm_crash_restart_limit: 3,
		process_stop_timeout: 5
	})

	// --- State from TemplateEditor ---
	const [templateValue, setTemplateValue] = useState('')
	const [templateLoading, setTemplateLoading] = useState(false)

	// --- State from RealmBinaryManager ---
	const [binaries, setBinaries] = useState<RealmBinary[]>([])
	const [binariesLoading, setBinariesLoading] = useState(false)
	const [uploading, setUploading] = useState(false)
	const [osValue, setOsValue] = useState('linux')
	const [archValue, setArchValue] = useState('x86_64')
	const [version, setVersion] = useState('')
	const [isDefault, setIsDefault] = useState(true)
	const [file, setFile] = useState<File | null>(null)

	// --- Logic from SettingsPanel ---
	const fetchSettings = async () => {
		setSettingsLoading(true)
		try {
			const res = await fetch('/api/v1/forwards/system-settings')
			if (!res.ok) throw new Error(`HTTP ${res.status}`)
			const body = await res.json()
			setSettings(body.data || settings)
		} catch (e: any) {
			toast.error(e?.message || 'Load failed')
		} finally {
			setSettingsLoading(false)
		}
	}

	const saveSettings = async () => {
		try {
			const res = await fetch('/api/v1/forwards/system-settings', {
				method: 'PUT',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify(settings)
			})
			if (!res.ok) throw new Error(`HTTP ${res.status}`)
			toast.success(t('forward.settingsSaved'))
		} catch (e: any) {
			toast.error(e?.message || 'Save failed')
		}
	}

	const updateSettingField = (key: keyof Settings, value: number | string) => {
		setSettings(prev => ({ ...prev, [key]: value }))
	}

	const periodOptions = [
		{ value: '10min', label: t('forward.historyPeriod10m', { defaultValue: '每10分钟' }) },
		{ value: '30min', label: t('forward.historyPeriod30m', { defaultValue: '每30分钟' }) },
		{ value: '1hour', label: t('forward.historyPeriod1h', { defaultValue: '每小时' }) },
		{ value: '1day', label: t('forward.historyPeriod1d', { defaultValue: '每天' }) }
	]

	// --- Logic from TemplateEditor ---
	const fetchTemplate = async () => {
		setTemplateLoading(true)
		try {
			const res = await fetch('/api/v1/forwards/realm/default-config')
			if (!res.ok) throw new Error(`HTTP ${res.status}`)
			const body = await res.json()
			setTemplateValue(body.data?.template_toml || '')
		} catch (e: any) {
			toast.error(e?.message || 'Load failed')
		} finally {
			setTemplateLoading(false)
		}
	}

	const saveTemplate = async () => {
		try {
			const res = await fetch('/api/v1/forwards/realm/default-config', {
				method: 'PUT',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ template_toml: templateValue })
			})
			if (!res.ok) throw new Error(`HTTP ${res.status}`)
			toast.success(t('forward.templateSaved'))
		} catch (e: any) {
			toast.error(e?.message || 'Save failed')
		}
	}

	// --- Logic from RealmBinaryManager ---
	const fetchBinaries = async () => {
		setBinariesLoading(true)
		try {
			const res = await fetch('/api/v1/realm/binaries')
			if (!res.ok) throw new Error(`HTTP ${res.status}`)
			const body = await res.json()
			setBinaries(body.data || [])
		} catch (e: any) {
			toast.error(e?.message || 'Load failed')
		} finally {
			setBinariesLoading(false)
		}
	}

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

	const handleFileChange = (e: React.ChangeEvent<HTMLInputElement>) => {
		const selectedFile = e.target.files?.[0] || null
		setFile(selectedFile)
		if (selectedFile) {
			const name = selectedFile.name
			// realm-aarch64-unknown-linux-gnu.tar.gz
			const match = name.match(/^realm-([a-z0-9_]+)-/)
			if (match && match[1]) {
				const arch = match[1]
				if (archOptions.includes(arch)) {
					setArchValue(arch)
				}
			}
		}
	}

	const handleUpload = async () => {
		if (!file) {
			toast.error(t('forward.realmBinarySelect', { defaultValue: '请选择文件' }))
			return
		}
		if (!version.trim()) {
			toast.error(t('forward.realmBinaryVersionRequired', { defaultValue: '版本号不能为空' }))
			return
		}
		const form = new FormData()
		form.append('os', osValue)
		form.append('arch', archValue)
		form.append('version', version.trim())
		form.append('is_default', String(isDefault))
		form.append('file', file)
		setUploading(true)
		try {
			const res = await fetch('/api/v1/realm/binaries', { method: 'POST', body: form })
			if (!res.ok) throw new Error(`HTTP ${res.status}`)
			toast.success(t('forward.realmBinaryUploaded', { defaultValue: '上传成功' }))
			setVersion('')
			setFile(null)
			fetchBinaries()
		} catch (e: any) {
			toast.error(e?.message || 'Upload failed')
		} finally {
			setUploading(false)
		}
	}

	const handleDelete = async (id: number) => {
		try {
			const res = await fetch(`/api/v1/realm/binaries/${id}`, { method: 'DELETE' })
			if (!res.ok) throw new Error(`HTTP ${res.status}`)
			toast.success(t('common.success'))
			fetchBinaries()
		} catch (e: any) {
			toast.error(e?.message || 'Delete failed')
		}
	}

	useEffect(() => {
		fetchSettings()
		fetchTemplate()
		fetchBinaries()
		// eslint-disable-next-line react-hooks/exhaustive-deps
	}, [])

	return (
		<Flex direction="column" gap="4">
			{/* --- SettingsPanel JSX --- */}
			<Card>
				<Flex justify="between" align="center" mb="3">
					<div>
						<Text weight="bold">{t('forward.systemSettings')}</Text>
						<Text size="1" color="gray" as="p">
							{t('forward.systemSettingsHint', { defaultValue: '修改后对所有规则立即生效' })}
						</Text>
					</div>
					<Button size="2" onClick={saveSettings} disabled={settingsLoading}>
						{t('forward.submit')}
					</Button>
				</Flex>
				<Grid columns={{ initial: '1', sm: '2' }} gap="4">
					<NumberField
						label={t('forward.statsReportInterval', { defaultValue: '统计上报间隔(秒)' })}
						help={t('forward.statsReportHint', { defaultValue: 'Agent 上报实时统计的时间间隔' })}
						min={10}
						max={300}
						value={settings.stats_report_interval}
						onChange={v => updateSettingField('stats_report_interval', v)}
					/>
					<NumberField
						label={t('forward.healthCheckInterval', { defaultValue: '健康检查间隔(秒)' })}
						help={t('forward.healthCheckHint', { defaultValue: 'Agent 执行链路健康检查的间隔' })}
						min={5}
						max={600}
						value={settings.health_check_interval}
						onChange={v => updateSettingField('health_check_interval', v)}
					/>
					<NumberField
						label={t('forward.crashRestartLimit', { defaultValue: '崩溃自动重启次数' })}
						help={t('forward.crashRestartHint', { defaultValue: '超过次数将停止重启并告警' })}
						min={1}
						max={10}
						value={settings.realm_crash_restart_limit}
						onChange={v => updateSettingField('realm_crash_restart_limit', v)}
					/>
					<NumberField
						label={t('forward.processStopTimeout', { defaultValue: '进程停止超时(秒)' })}
						help={t('forward.processStopHint', { defaultValue: 'SIGTERM 后等待进程优雅退出的时间' })}
						min={3}
						max={30}
						value={settings.process_stop_timeout}
						onChange={v => updateSettingField('process_stop_timeout', v)}
					/>
					<div className="sm:col-span-2 flex flex-col gap-2">
						<Text size="2">{t('forward.historyAggregatePeriod', { defaultValue: '历史数据聚合周期' })}</Text>
						<Text size="1" color="gray">
							{t('forward.historyAggregateHint', { defaultValue: '历史流量数据的聚合粒度' })}
						</Text>
						<Select.Root
							value={settings.history_aggregate_period}
							onValueChange={v => updateSettingField('history_aggregate_period', v)}
						>
							<Select.Trigger />
							<Select.Content>
								{periodOptions.map(option => (
									<Select.Item key={option.value} value={option.value}>
										{option.label}
									</Select.Item>
								))}
							</Select.Content>
						</Select.Root>
					</div>
				</Grid>
			</Card>

			{/* --- TemplateEditor JSX --- */}
			<Card>
				<Flex justify="between" align="center" mb="3">
					<Text weight="bold">{t('forward.realmTemplate')}</Text>
					<Flex gap="2">
						<Button variant="ghost" onClick={fetchTemplate} disabled={templateLoading}>
							{t('forward.refresh')}
						</Button>
						<Button onClick={saveTemplate} disabled={templateLoading}>
							{t('forward.submit')}
						</Button>
					</Flex>
				</Flex>
				<TextArea minRows={12} value={templateValue} onChange={e => setTemplateValue(e.target.value)} />
			</Card>

			{/* --- RealmBinaryManager JSX --- */}
			<Card>
				<Flex justify="between" align="center" mb="3">
					<Text weight="bold">{t('forward.realmBinaryTitle', { defaultValue: 'Realm二进制文件管理' })}</Text>
					<Button variant="ghost" onClick={fetchBinaries} disabled={binariesLoading}>
						{t('forward.refresh')}
					</Button>
				</Flex>

				<Table.Root>
					<Table.Header>
						<Table.Row>
							<Table.ColumnHeaderCell>{t('forward.os', { defaultValue: '系统' })}</Table.ColumnHeaderCell>
							<Table.ColumnHeaderCell>{t('forward.arch', { defaultValue: '架构' })}</Table.ColumnHeaderCell>
							<Table.ColumnHeaderCell>{t('forward.version', { defaultValue: '版本' })}</Table.ColumnHeaderCell>
							<Table.ColumnHeaderCell>{t('forward.size', { defaultValue: '大小' })}</Table.ColumnHeaderCell>
							<Table.ColumnHeaderCell>{t('forward.default', { defaultValue: '默认' })}</Table.ColumnHeaderCell>
							<Table.ColumnHeaderCell>{t('forward.actions')}</Table.ColumnHeaderCell>
						</Table.Row>
					</Table.Header>
					<Table.Body>
						{binaries.length === 0 ? (
							<Table.Row>
								<Table.Cell colSpan={6}>
									<Text size="2" color="gray">
										{t('forward.realmBinaryEmpty', { defaultValue: '暂无二进制文件' })}
									</Text>
								</Table.Cell>
							</Table.Row>
						) : (
							binaries.map(item => (
								<Table.Row key={item.id}>
									<Table.Cell>{item.os}</Table.Cell>
									<Table.Cell>{item.arch}</Table.Cell>
									<Table.Cell>{item.version}</Table.Cell>
									<Table.Cell>{formatBytes(item.file_size)}</Table.Cell>
									<Table.Cell>
										{item.is_default ? <Badge color="green">{t('common.default', { defaultValue: '默认' })}</Badge> : '-'}
									</Table.Cell>
									<Table.Cell>
										<Flex gap="2">
											<Button
												size="1"
												variant="soft"
												onClick={() => window.open(`/api/v1/realm/binaries/${item.id}/download`, '_blank')}
											>
												{t('forward.download', { defaultValue: '下载' })}
											</Button>
											<Button size="1" variant="soft" color="red" onClick={() => handleDelete(item.id)}>
												{t('forward.delete', { defaultValue: '删除' })}
											</Button>
										</Flex>
									</Table.Cell>
								</Table.Row>
							))
						)}
					</Table.Body>
				</Table.Root>

				<Card className="mt-4 p-3">
					<Text size="2" weight="bold">
						{t('forward.realmBinaryUpload', { defaultValue: '上传新版本' })}
					</Text>
					<Grid columns={{ initial: '1', sm: '2' }} gap="3" mt="3">
						<div className="flex flex-col gap-2">
							<Text size="2">{t('forward.os', { defaultValue: '系统' })}</Text>
							<Select.Root value={osValue} onValueChange={setOsValue} disabled>
								<Select.Trigger />
								<Select.Content>
									{osOptions.map(os => (
										<Select.Item key={os} value={os}>
											{os}
										</Select.Item>
									))}
								</Select.Content>
							</Select.Root>
						</div>
						<div className="flex flex-col gap-2">
							<Text size="2">{t('forward.arch', { defaultValue: '架构' })}</Text>
							<Select.Root value={archValue} onValueChange={setArchValue}>
								<Select.Trigger />
								<Select.Content>
									{archOptions.map(arch => (
										<Select.Item key={arch} value={arch}>
											{arch}
										</Select.Item>
									))}
								</Select.Content>
							</Select.Root>
						</div>
						<div className="flex flex-col gap-2">
							<Text size="2">{t('forward.version', { defaultValue: '版本' })}</Text>
							<TextField.Root value={version} onChange={e => setVersion(e.target.value)} placeholder="2.6.0" />
						</div>
						<div className="flex flex-col gap-2">
							<Text size="2">{t('forward.file', { defaultValue: '文件' })}</Text>
							<TextField.Root
								type="file"
								onChange={handleFileChange}
								accept=".tar.gz"
								className="block w-full text-sm text-slate-500 file:mr-4 file:py-2 file:px-4 file:rounded-full file:border-0 file:text-sm file:font-semibold file:bg-violet-50 file:text-violet-700 hover:file:bg-violet-100"
							/>
						</div>
					</Grid>
					<Flex align="center" gap="2" mt="3">
						<label className="flex items-center gap-2 text-sm">
							<input type="checkbox" checked={isDefault} onChange={e => setIsDefault(e.target.checked)} />
							{t('forward.default', { defaultValue: '默认版本' })}
						</label>
					</Flex>
					<Button className="mt-3" onClick={handleUpload} disabled={uploading}>
						{t('forward.upload', { defaultValue: '上传' })}
					</Button>
				</Card>
			</Card>
		</Flex>
	)
}

export default SettingsAndTemplate
