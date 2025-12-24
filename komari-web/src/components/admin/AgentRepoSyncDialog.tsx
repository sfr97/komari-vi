import { Badge, Button, Card, Checkbox, Flex, ScrollArea, Switch, Text, TextArea, TextField } from '@radix-ui/themes'
import * as React from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'
import { Check, X } from 'lucide-react'

type PreviewAsset = {
	asset_id: number
	name: string
	size: number
	download_url: string
	content_type: string
	is_valid: boolean
	os?: string
	arch?: string
}

type PreviewRelease = {
	release_id: number
	name: string
	tag_name: string
	body: string
	prerelease: boolean
	published_at?: string | null
	assets: PreviewAsset[]
}

type PreviewResponse = {
	repo: string
	keyword: string
	releases: PreviewRelease[]
}

type StartResponse = {
	session_id: string
}

type RepoSyncProgress = {
	current_file: string
	file_downloaded: number
	file_total: number
	overall_downloaded: number
	overall_total: number
	index: number
	count: number
}

type RepoSyncFileState = {
	asset_id: number
	name: string
	state: 'pending' | 'downloading' | 'done' | 'error'
	downloaded?: number
	total?: number
	error?: string
}

async function extractError(resp: Response) {
	try {
		const data = await resp.json()
		return data?.message || data?.error || resp.statusText
	} catch {
		return resp.statusText
	}
}

const formatBytes = (bytes: number) => {
	if (!bytes || bytes <= 0) return '0B'
	const units = ['B', 'KB', 'MB', 'GB']
	const i = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1)
	const value = bytes / Math.pow(1024, i)
	return `${value.toFixed(value >= 10 || value % 1 === 0 ? 0 : 1)}${units[i]}`
}

function normalizeProxy(input: string) {
	const s = (input || '').trim()
	if (!s) return ''
	if (s.startsWith('http://') || s.startsWith('https://')) return s.replace(/\/+$/, '')
	return `https://${s}`.replace(/\/+$/, '')
}

export function AgentRepoSyncDialog({ onSuccess, onClose }: { onSuccess: () => void; onClose: () => void }) {
	const { t } = useTranslation()
	const [step, setStep] = React.useState<1 | 2>(1)

	const [repoInput, setRepoInput] = React.useState('')
	const [keyword, setKeyword] = React.useState('agent')
	const [includePrerelease, setIncludePrerelease] = React.useState(false)
	const [proxy, setProxy] = React.useState('')

	const [loadingPreview, setLoadingPreview] = React.useState(false)
	const [preview, setPreview] = React.useState<PreviewResponse | null>(null)

	const [expandedReleaseId, setExpandedReleaseId] = React.useState<number | null>(null)
	const [selectedReleaseId, setSelectedReleaseId] = React.useState<number | null>(null)
	const [selectedAssetIds, setSelectedAssetIds] = React.useState<number[]>([])
	const [setCurrent, setSetCurrent] = React.useState(true)

	const [syncing, setSyncing] = React.useState(false)
	const [progress, setProgress] = React.useState<RepoSyncProgress | null>(null)
	const [fileStates, setFileStates] = React.useState<Record<number, RepoSyncFileState>>({})
	const esRef = React.useRef<EventSource | null>(null)

	React.useEffect(() => {
		return () => {
			esRef.current?.close()
		}
	}, [])

	const resetSelection = (releaseId: number | null) => {
		setSelectedReleaseId(releaseId)
		setSelectedAssetIds([])
		setProgress(null)
		setFileStates({})
	}

	const fetchPreview = async () => {
		const repo = repoInput.trim()
		if (!repo) {
			toast.error(t('agent_version.repo_sync_repo_required', '请填写 GitHub 仓库地址'))
			return false
		}
		const kw = keyword.trim() || 'agent'
		setLoadingPreview(true)
		try {
			const resp = await fetch('/api/admin/agent-version/repo-sync/preview', {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({
					repo,
					keyword: kw,
					include_prerelease: includePrerelease
				})
			})
			if (!resp.ok) throw new Error(await extractError(resp))
			const data = await resp.json()
			const payload: PreviewResponse = data?.data
			setPreview(payload)
			setStep(2)
			setExpandedReleaseId(payload?.releases?.[0]?.release_id ?? null)
			resetSelection(payload?.releases?.[0]?.release_id ?? null)
			return true
		} catch (err: any) {
			toast.error(t('agent_version.operation_failed') + ': ' + (err?.message || err))
			return false
		} finally {
			setLoadingPreview(false)
		}
	}

	const startSync = async () => {
		if (!preview) return
		if (!selectedReleaseId) {
			toast.error(t('agent_version.repo_sync_release_required', '请选择要同步的版本'))
			return
		}
		if (selectedAssetIds.length === 0) {
			toast.error(t('agent_version.repo_sync_assets_required', '请选择要同步的包'))
			return
		}

		setSyncing(true)
		setProgress(null)
		setFileStates(() => {
			const next: Record<number, RepoSyncFileState> = {}
			for (const id of selectedAssetIds) {
				next[id] = { asset_id: id, name: '', state: 'pending' }
			}
			return next
		})
		try {
			const resp = await fetch('/api/admin/agent-version/repo-sync/start', {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({
					repo: preview.repo,
					release_id: selectedReleaseId,
					asset_ids: selectedAssetIds,
					set_current: setCurrent,
					proxy: normalizeProxy(proxy)
				})
			})
			if (!resp.ok) throw new Error(await extractError(resp))
			const data = await resp.json()
			const payload: StartResponse = data?.data
			if (!payload?.session_id) throw new Error('missing session_id')

			const es = new EventSource(`/api/admin/agent-version/repo-sync/${payload.session_id}/stream`)
			esRef.current = es

			es.addEventListener('file', ev => {
				try {
					const st = JSON.parse((ev as MessageEvent).data || '{}') as RepoSyncFileState
					if (!st?.asset_id) return
					setFileStates(prev => ({
						...prev,
						[st.asset_id]: {
							...prev[st.asset_id],
							...st,
							state: (st.state as any) || prev[st.asset_id]?.state || 'pending'
						}
					}))
				} catch {
					// ignore
				}
			})
			es.addEventListener('progress', ev => {
				try {
					const p = JSON.parse((ev as MessageEvent).data || '{}')
					setProgress(p)
					const cur = String(p?.current_file || '')
					if (!cur) return
					const release = preview.releases.find(x => x.release_id === selectedReleaseId)
					const match = release?.assets?.find(x => x.name === cur)
					if (!match) return
					setFileStates(prev => ({
						...prev,
						[match.asset_id]: {
							asset_id: match.asset_id,
							name: match.name,
							state: 'downloading',
							downloaded: Number(p?.file_downloaded || 0),
							total: Number(p?.file_total || match.size || 0)
						}
					}))
				} catch {
					// ignore
				}
			})
			es.addEventListener('done', ev => {
				es.close()
				esRef.current = null
				let ok = true
				let error = ''
				try {
					const d = JSON.parse((ev as MessageEvent).data || '{}')
					ok = Boolean(d?.success)
					error = String(d?.error || '')
				} catch {
					ok = true
				}
				setSyncing(false)
				if (ok) {
					setFileStates(prev => {
						const next = { ...prev }
						for (const id of selectedAssetIds) {
							if (!next[id]) next[id] = { asset_id: id, name: '', state: 'done' }
							if (next[id].state !== 'error') next[id] = { ...next[id], state: 'done' }
						}
						return next
					})
					toast.success(t('agent_version.repo_sync_done', '同步完成'))
					onSuccess()
				} else {
					// 如果没有明确的 file error，就把当前文件标记为 error
					const cur = progress?.current_file
					if (cur) {
						const release = preview.releases.find(x => x.release_id === selectedReleaseId)
						const match = release?.assets?.find(x => x.name === cur)
						if (match) {
							setFileStates(prev => ({
								...prev,
								[match.asset_id]: {
									asset_id: match.asset_id,
									name: match.name,
									state: 'error',
									error: error || 'unknown error'
								}
							}))
						}
					}
					toast.error(t('agent_version.operation_failed') + ': ' + (error || 'unknown error'))
				}
			})
			es.onerror = () => {
				// 连接中断时，交由 done/用户刷新处理
			}
		} catch (err: any) {
			setSyncing(false)
			toast.error(t('agent_version.operation_failed') + ': ' + (err?.message || err))
		}
	}

	const releases = preview?.releases || []

	const overallPercent = React.useMemo(() => {
		if (!progress) return 0
		if (progress.overall_total > 0) return Math.min(100, Math.round((progress.overall_downloaded / progress.overall_total) * 100))
		if (progress.count > 0) return Math.min(100, Math.round((progress.index / progress.count) * 100))
		return 0
	}, [progress])

	const renderAssetRight = (a: PreviewAsset) => {
		const st = fileStates[a.asset_id]
		if (st?.state === 'done') {
			return (
				<span className="shrink-0 text-green-600" title={t('agent_version.repo_sync_done', '同步完成') as string}>
					<Check size={16} />
				</span>
			)
		}
		if (st?.state === 'error') {
			return (
				<span className="shrink-0 text-red-600" title={st.error || t('agent_version.operation_failed', '操作失败') as string}>
					<X size={16} />
				</span>
			)
		}
		if (st?.state === 'downloading') {
			const dl = Number(st.downloaded || 0)
			const total = Number(st.total || a.size || 0)
			return (
				<Text size="1" color="gray" className="shrink-0">
					{formatBytes(dl)}
					{total > 0 ? ` / ${formatBytes(total)}` : ''}
				</Text>
			)
		}
		return (
			<Text size="1" color="gray" className="shrink-0">
				{formatBytes(a.size)}
			</Text>
		)
	}

	return (
		<div className="flex flex-col gap-4">
			<Text size="5" weight="bold">
				{t('agent_version.repo_sync', '同步仓库')}
			</Text>
			<Text size="2" color="gray">
				{t('agent_version.repo_sync_hint', '从 GitHub Releases 拉取版本与安装包，并同步到本地版本库。')}
			</Text>

			{step === 1 ? (
				<Card>
					<div className="flex flex-col gap-3">
						<label className="flex flex-col gap-2">
							<Text size="2" weight="medium">
								{t('agent_version.repo_sync_repo', 'GitHub 仓库')} <Text color="red">*</Text>
							</Text>
							<TextField.Root
								value={repoInput}
								onChange={e => setRepoInput(e.target.value)}
								placeholder="danger-dream/komari-vi 或 https://github.com/danger-dream/komari-vi"
								disabled={loadingPreview || syncing}
							/>
						</label>
						<label className="flex flex-col gap-2">
							<Text size="2" weight="medium">{t('agent_version.repo_sync_keyword', '关键字')}</Text>
							<TextField.Root
								value={keyword}
								onChange={e => setKeyword(e.target.value)}
								placeholder="agent"
								disabled={loadingPreview || syncing}
							/>
							<Text size="1" color="gray">
								{t('agent_version.repo_sync_keyword_hint', '用于过滤 release（name/tag_name）')}
							</Text>
						</label>

						<Flex align="center" justify="between" className="p-3 rounded-lg" style={{ backgroundColor: 'var(--accent-3)' }}>
							<div>
								<Text size="2" weight="medium">
									{t('agent_version.repo_sync_include_prerelease', '包含预发布')}
								</Text>
								<Text size="1" color="gray" className="block mt-1">
									{t('agent_version.repo_sync_include_prerelease_desc', '默认不包含 prerelease')}
								</Text>
							</div>
							<Switch checked={includePrerelease} onCheckedChange={setIncludePrerelease} disabled={loadingPreview || syncing} />
						</Flex>

						<label className="flex flex-col gap-2">
							<Text size="2" weight="medium">{t('agent_version.repo_sync_proxy', '下载代理（可选）')}</Text>
							<TextField.Root
								value={proxy}
								onChange={e => setProxy(e.target.value)}
								placeholder="https://ghfast.top"
								disabled={loadingPreview || syncing}
							/>
							<Text size="1" color="gray">
								{t('agent_version.repo_sync_proxy_hint', '下载时将使用：代理地址/原始下载地址')}
							</Text>
						</label>

						<Flex gap="2" justify="end">
							<Button variant="soft" color="gray" onClick={onClose} disabled={loadingPreview || syncing}>
								{t('common.cancel')}
							</Button>
							<Button onClick={fetchPreview} disabled={loadingPreview || syncing}>
								{loadingPreview ? t('common.loading', '加载中...') : t('common.next', '下一步')}
							</Button>
						</Flex>
					</div>
				</Card>
			) : (
				<div className="flex flex-col gap-3">
					<Card>
						<Flex justify="between" align="center" gap="2">
							<div className="min-w-0">
								<Text size="2" weight="medium" className="truncate">
									{t('agent_version.repo_sync_repo', 'GitHub 仓库')}: <Text className="font-mono">{preview?.repo}</Text>
								</Text>
								<Text size="1" color="gray">
									{t('agent_version.repo_sync_versions', '共 {{count}} 个版本', { count: releases.length })}
								</Text>
							</div>
							<Flex gap="2" className="shrink-0">
								<Button variant="soft" onClick={() => setStep(1)} disabled={syncing}>
									{t('common.prev', '上一步')}
								</Button>
								<Button variant="soft" onClick={fetchPreview} disabled={loadingPreview || syncing}>
									{t('agent_version.refresh')}
								</Button>
							</Flex>
						</Flex>
					</Card>

					<Card>
						{releases.length === 0 ? (
							<Text color="gray">{t('agent_version.repo_sync_empty', '没有找到匹配的 Releases')}</Text>
						) : (
							<div className="flex flex-col gap-2">
								{releases.map(r => {
									const isExpanded = r.release_id === expandedReleaseId
									const isSelected = r.release_id === selectedReleaseId
									return (
										<div
											key={r.release_id}
											className={`rounded-lg border p-3 ${isExpanded ? 'border-[var(--accent-8)]' : 'border-[var(--gray-a4)]'}`}
											style={{ backgroundColor: isExpanded ? 'var(--accent-2)' : 'transparent' }}>
											<button
												type="button"
												className="w-full text-left"
												disabled={syncing}
												onClick={() => {
													const next = isExpanded ? null : r.release_id
													setExpandedReleaseId(next)
													resetSelection(next)
												}}>
												<Flex justify="between" align="center" gap="2">
													<div className="min-w-0">
														<Flex gap="2" align="center" wrap="wrap">
															<Text weight="bold" className="truncate">
																{r.tag_name || r.name}
															</Text>
															{r.prerelease && <Badge color="orange">{t('agent_version.repo_sync_prerelease', '预发布')}</Badge>}
															{isSelected && <Badge color="green">{t('agent_version.repo_sync_selected', '已选择')}</Badge>}
														</Flex>
														<Text size="1" color="gray" className="truncate">
															{r.name}
														</Text>
													</div>
													<Text size="1" color="gray">
														{t('agent_version.repo_sync_assets_count', '{{count}} 个包', { count: r.assets?.length || 0 })}
													</Text>
												</Flex>
											</button>

											{isExpanded && (
												<div className="mt-3 flex flex-col gap-2">
													<div className="flex flex-col gap-1">
														{(r.assets || []).map(a => {
															const checked = selectedAssetIds.includes(a.asset_id)
															const disabled = syncing || !a.is_valid
															return (
																<label
																	key={a.asset_id}
																	className={`flex items-center gap-2 rounded-md px-2 py-1 ${
																		disabled ? 'opacity-60' : 'hover:bg-[var(--accent-a2)]'
																	}`}>
																	<Checkbox
																		checked={checked}
																		disabled={disabled}
																		onCheckedChange={v => {
																			const isChecked = Boolean(v)
																			setSelectedAssetIds(prev => {
																				if (isChecked) return Array.from(new Set([...prev, a.asset_id]))
																				return prev.filter(id => id !== a.asset_id)
																			})
																		}}
																	/>
																	<div className="min-w-0 flex-1">
																		<Flex gap="2" align="center" wrap="wrap" className="min-w-0">
																			<Text size="1" className="truncate font-mono max-w-full">
																				{a.name}
																			</Text>
																			{a.is_valid ? (
																				<Badge color="blue" size="1">
																					{a.os}/{a.arch}
																				</Badge>
																			) : (
																				<Badge color="gray" size="1">
																					{t('agent_version.repo_sync_invalid', '不可用')}
																				</Badge>
																			)}
																		</Flex>
																	</div>
																	{renderAssetRight(a)}
																</label>
															)
														})}
													</div>

													{r.body?.trim() && (
														<div className="mt-1">
															<Text size="2" weight="medium">
																{t('agent_version.changelog', '更新内容')}
															</Text>
															<ScrollArea type="always" scrollbars="vertical" style={{ height: 120, marginTop: 6 }}>
																<TextArea value={r.body} readOnly rows={6} />
															</ScrollArea>
														</div>
													)}
												</div>
											)}
										</div>
									)
								})}
							</div>
						)}
					</Card>

					<Card>
						<Flex align="center" justify="between" className="p-3 rounded-lg" style={{ backgroundColor: 'var(--accent-3)' }}>
							<div>
								<Text size="2" weight="medium">
									{t('agent_version.is_current', '设为当前版本')}
								</Text>
								<Text size="1" color="gray" className="block mt-1">
									{t('agent_version.repo_sync_set_current_desc', '同步完成后，将该版本标记为当前版本')}
								</Text>
							</div>
							<Switch checked={setCurrent} onCheckedChange={setSetCurrent} disabled={syncing} />
						</Flex>

						{syncing && (
							<div className="mt-3 flex flex-col gap-2">
								<Text size="2" weight="medium">
									{t('agent_version.repo_sync_progress', '同步进度')}
								</Text>
								<div className="h-2 w-full rounded bg-[var(--gray-a3)] overflow-hidden">
									<div className="h-2 bg-[var(--accent-9)]" style={{ width: `${overallPercent}%` }} />
								</div>
								<Text size="1" color="gray">
									{progress?.current_file
										? `${progress.current_file}  (${formatBytes(progress.file_downloaded)} / ${formatBytes(progress.file_total || 0)})`
										: ''}
								</Text>
								<Text size="1" color="gray">
									{progress
										? `${t('agent_version.repo_sync_overall', '总计')}: ${formatBytes(progress.overall_downloaded)} / ${formatBytes(progress.overall_total || 0)} (${overallPercent}%)`
										: ''}
								</Text>
							</div>
						)}

						<Flex gap="2" justify="end" className="mt-3">
							<Button variant="soft" color="gray" onClick={onClose} disabled={syncing}>
								{t('common.cancel')}
							</Button>
							<Button onClick={startSync} disabled={syncing || loadingPreview}>
								{syncing ? t('agent_version.repo_sync_syncing', '同步中...') : t('agent_version.repo_sync_start', '开始同步')}
							</Button>
						</Flex>
					</Card>
				</div>
			)}
		</div>
	)
}
