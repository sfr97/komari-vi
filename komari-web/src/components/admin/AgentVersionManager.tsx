import Loading from '@/components/loading'
import { Badge, Button, Card, Dialog, Flex, IconButton, Switch, Text, TextArea, TextField } from '@radix-ui/themes'
import { useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'
import { Pencil, Trash, Upload, Plus, Download, Check } from 'lucide-react'
import { AgentRepoSyncDialog } from '@/components/admin/AgentRepoSyncDialog'

type AgentPackage = {
	id: number
	os: string
	arch: string
	file_name: string
	file_size: number
	created_at: string
}

type AgentVersion = {
	id: number
	version: string
	changelog?: string
	is_current: boolean
	created_at: string
	packages?: AgentPackage[]
}

const formatBytes = (bytes: number) => {
	if (!bytes || bytes <= 0) return '0B'
	const units = ['B', 'KB', 'MB', 'GB']
	const i = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1)
	const value = bytes / Math.pow(1024, i)
	return `${value.toFixed(value >= 10 || value % 1 === 0 ? 0 : 1)}${units[i]}`
}

async function extractError(resp: Response) {
	try {
		const data = await resp.json()
		return data?.message || data?.error || resp.statusText
	} catch {
		return resp.statusText
	}
}

export function AgentVersionManager() {
	const { t } = useTranslation()
	const [versions, setVersions] = useState<AgentVersion[]>([])
	const [loading, setLoading] = useState<boolean>(true)
	const [createOpen, setCreateOpen] = useState(false)
	const [repoSyncOpen, setRepoSyncOpen] = useState(false)

	const fetchVersions = async () => {
		setLoading(true)
		try {
			const resp = await fetch('/api/admin/agent-version/')
			if (!resp.ok) throw new Error(await extractError(resp))
			const data = await resp.json()
			setVersions(data?.data ?? [])
		} catch (error: any) {
			toast.error(t('agent_version.fetch_failed') + ': ' + (error?.message || error))
		} finally {
			setLoading(false)
		}
	}

	useEffect(() => {
		fetchVersions()
	}, [])

	if (loading) return <Loading />

	return (
		<Flex direction="column" gap="4">
			<Flex justify="between" align="center">
				<div>
					<Text size="5" weight="bold">
						{t('agent_version.title')}
					</Text>
					<Text size="2" color="gray" className="block mt-1">
						{t('agent_version.form_hint')}
					</Text>
				</div>
				<Flex gap="2">
					<Button variant="soft" onClick={fetchVersions}>
						{t('agent_version.refresh')}
					</Button>
					<Dialog.Root open={repoSyncOpen} onOpenChange={open => setRepoSyncOpen(open)}>
						<Dialog.Trigger>
							<Button variant="soft">
								{t('agent_version.repo_sync', '同步仓库')}
							</Button>
						</Dialog.Trigger>
						<Dialog.Content
							style={{ maxWidth: 760 }}
							onPointerDownOutside={e => e.preventDefault()}
							onEscapeKeyDown={e => e.preventDefault()}>
							<AgentRepoSyncDialog
								onSuccess={() => {
									fetchVersions()
									setRepoSyncOpen(false)
								}}
								onClose={() => setRepoSyncOpen(false)}
							/>
						</Dialog.Content>
					</Dialog.Root>
					<Dialog.Root open={createOpen} onOpenChange={setCreateOpen}>
						<Dialog.Trigger>
							<Button>
								<Plus size={16} />
								{t('agent_version.create')}
							</Button>
						</Dialog.Trigger>
						<Dialog.Content style={{ maxWidth: 600 }}>
							<CreateVersionDialog onSuccess={fetchVersions} onClose={() => setCreateOpen(false)} />
						</Dialog.Content>
					</Dialog.Root>
				</Flex>
			</Flex>

			<Card>
				{versions.length === 0 ? (
					<div className="text-center py-8">
						<Text color="gray">{t('agent_version.empty')}</Text>
					</div>
				) : (
					<div className="grid grid-cols-1 xl:grid-cols-2 gap-3">
						{versions.map(v => (
							<VersionCard key={v.id} version={v} onUpdate={fetchVersions} />
						))}
					</div>
				)}
			</Card>
		</Flex>
	)
}

const CreateVersionDialog = ({ onSuccess, onClose }: { onSuccess: () => void; onClose: () => void }) => {
	const { t } = useTranslation()
	const [submitting, setSubmitting] = useState(false)
	const [formVersion, setFormVersion] = useState('')
	const [formChangelog, setFormChangelog] = useState('')
	const [formIsCurrent, setFormIsCurrent] = useState(true)
	const [formFiles, setFormFiles] = useState<File[]>([])
	const fileInputRef = useRef<HTMLInputElement | null>(null)

	const handleCreate = async (e: React.FormEvent<HTMLFormElement>) => {
		e.preventDefault()
		if (!formVersion.trim()) {
			toast.error(t('agent_version.version_required'))
			return
		}
		if (formFiles.length === 0) {
			toast.error(t('agent_version.files_required'))
			return
		}
		setSubmitting(true)
		const fd = new FormData()
		fd.append('version', formVersion.trim())
		fd.append('changelog', formChangelog)
		fd.append('is_current', String(formIsCurrent))
		formFiles.forEach(file => fd.append('files', file))
		try {
			const resp = await fetch('/api/admin/agent-version/', {
				method: 'POST',
				body: fd
			})
			if (!resp.ok) {
				throw new Error(await extractError(resp))
			}
			toast.success(t('agent_version.toast_created'))
			onSuccess()
			onClose()
		} catch (error: any) {
			toast.error(t('agent_version.operation_failed') + ': ' + (error?.message || error))
		} finally {
			setSubmitting(false)
		}
	}

	return (
		<form onSubmit={handleCreate} className="flex flex-col gap-4">
			<Dialog.Title>{t('agent_version.create')}</Dialog.Title>

			<label className="flex flex-col gap-2">
				<Text size="2" weight="medium">
					{t('agent_version.version')} <Text color="red">*</Text>
				</Text>
				<TextField.Root placeholder="1.2.3" value={formVersion} onChange={e => setFormVersion(e.target.value)} required />
			</label>

			<label className="flex flex-col gap-2">
				<Text size="2" weight="medium">
					{t('agent_version.changelog')} <Text size="1" color="gray">({t('common.optional', '可选')})</Text>
				</Text>
				<TextArea placeholder={t('agent_version.changelog_placeholder')} value={formChangelog} onChange={e => setFormChangelog(e.target.value)} rows={4} />
			</label>

			<Flex align="center" justify="between" className="p-3 rounded-lg" style={{ backgroundColor: 'var(--accent-3)' }}>
				<div>
					<Text size="2" weight="medium">
						{t('agent_version.is_current')}
					</Text>
					<Text size="1" color="gray" className="block mt-1">
						{t('agent_version.is_current_desc')}
					</Text>
				</div>
				<Switch checked={formIsCurrent} onCheckedChange={setFormIsCurrent} />
			</Flex>

			<label className="flex flex-col gap-2">
				<Flex justify="between" align="center">
					<Text size="2" weight="medium">
						{t('agent_version.files')} <Text color="red">*</Text>
					</Text>
					<Text size="1" color="gray">
						{t('agent_version.upload_hint')}
					</Text>
				</Flex>
				<input
					ref={fileInputRef}
					type="file"
					multiple
					onChange={e => setFormFiles(Array.from(e.target.files || []))}
					className="block w-full cursor-pointer rounded-lg border border-[var(--accent-6)] bg-[var(--accent-2)] px-3 py-2 text-sm text-[var(--accent-12)] file:mr-3 file:rounded-md file:border-0 file:bg-[var(--accent-9)] file:px-3 file:py-1 file:text-white hover:file:bg-[var(--accent-10)]"
				/>
				{formFiles.length > 0 && (
					<Flex gap="2" wrap="wrap">
						{formFiles.map(file => (
							<Badge key={file.name} color="blue">
								{file.name}
							</Badge>
						))}
					</Flex>
				)}
			</label>

			<Flex gap="2" justify="end" className="mt-2">
				<Dialog.Close>
					<Button type="button" variant="soft" color="gray">
						{t('common.cancel')}
					</Button>
				</Dialog.Close>
				<Button type="submit" disabled={submitting}>
					{submitting ? t('agent_version.uploading') : t('agent_version.submit')}
				</Button>
			</Flex>
		</form>
	)
}

const VersionCard = ({ version, onUpdate }: { version: AgentVersion; onUpdate: () => void }) => {
	const { t } = useTranslation()
	const [editOpen, setEditOpen] = useState(false)
	const [deleteOpen, setDeleteOpen] = useState(false)
	const [uploadOpen, setUploadOpen] = useState(false)
	const [actingId, setActingId] = useState<number | null>(null)

	const markCurrent = async () => {
		setActingId(version.id)
		try {
			const resp = await fetch(`/api/admin/agent-version/${version.id}/metadata`, {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ is_current: true })
			})
			if (!resp.ok) throw new Error(await extractError(resp))
			toast.success(t('agent_version.toast_mark_current'))
			onUpdate()
		} catch (error: any) {
			toast.error(t('agent_version.operation_failed') + ': ' + (error?.message || error))
		} finally {
			setActingId(null)
		}
	}

	const handleDelete = async () => {
		setActingId(version.id)
		try {
			const resp = await fetch(`/api/admin/agent-version/${version.id}`, {
				method: 'DELETE'
			})
			if (!resp.ok) throw new Error(await extractError(resp))
			toast.success(t('agent_version.toast_deleted'))
			setDeleteOpen(false)
			onUpdate()
		} catch (error: any) {
			toast.error(t('agent_version.operation_failed') + ': ' + (error?.message || error))
		} finally {
			setActingId(null)
		}
	}

	return (
		<>
			<div className="rounded-lg border border-[var(--gray-a4)] p-4 bg-[var(--panel-solid)]">
				<Flex justify="between" align="start" gap="3">
					<div className="min-w-0">
						<Flex gap="2" align="center" wrap="wrap">
							<Text weight="bold" size="4" className="truncate">
								{version.version}
							</Text>
							{version.is_current && (
								<Badge color="green" size="1">
									{t('agent_version.current_badge')}
								</Badge>
							)}
						</Flex>
						<Text size="1" color="gray" className="block mt-1">
							{t('agent_version.created_at')}: {version.created_at ? new Date(version.created_at).toLocaleString() : '--'}
						</Text>
					</div>

					<Flex gap="2" className="shrink-0">
						<IconButton
							size="2"
							variant="soft"
							color="green"
							onClick={markCurrent}
							disabled={actingId === version.id || version.is_current}
							title={
								version.is_current
									? (t('agent_version.current_badge') as string)
									: (t('agent_version.mark_current') as string)
							}>
							<Check size={16} />
						</IconButton>
						<IconButton size="2" variant="soft" onClick={() => setUploadOpen(true)} title={t('agent_version.upload_more')}>
							<Upload size={16} />
						</IconButton>
						<IconButton size="2" variant="soft" onClick={() => setEditOpen(true)} title={t('common.edit')}>
							<Pencil size={16} />
						</IconButton>
						<IconButton
							size="2"
							variant="soft"
							color={version.is_current ? 'gray' : 'red'}
							onClick={() => !version.is_current && setDeleteOpen(true)}
							disabled={version.is_current}
							title={
								version.is_current
									? (t('agent_version.cannot_delete_current', '不能删除当前版本') as string)
									: (t('common.delete') as string)
							}>
							<Trash size={16} />
						</IconButton>
					</Flex>
				</Flex>

				<div className="mt-3">
					<Text size="2" weight="medium">
						{t('agent_version.changelog')}
					</Text>
					<Text size="2" color="gray" className="line-clamp-3 mt-1">
						{version.changelog?.trim() ? version.changelog : t('agent_version.changelog_empty')}
					</Text>
				</div>

				<div className="mt-3">
					<Text size="2" weight="medium">
						{t('agent_version.packages_title')}
					</Text>
					{version.packages && version.packages.length > 0 ? (
						<div className="grid grid-cols-1 sm:grid-cols-2 gap-2 mt-2">
							{version.packages.map(p => (
								<PackageTile key={p.id} pkg={p} versionId={version.id} onUpdate={onUpdate} />
							))}
						</div>
					) : (
						<Text size="1" color="gray" className="mt-2">
							{t('agent_version.no_packages')}
						</Text>
					)}
				</div>
			</div>

			<Dialog.Root open={editOpen} onOpenChange={setEditOpen}>
				<Dialog.Content style={{ maxWidth: 600 }}>
					<EditVersionDialog version={version} onSuccess={onUpdate} onClose={() => setEditOpen(false)} />
				</Dialog.Content>
			</Dialog.Root>

			<Dialog.Root open={deleteOpen} onOpenChange={setDeleteOpen}>
				<Dialog.Content style={{ maxWidth: 450 }}>
					<Dialog.Title>{t('common.delete')}</Dialog.Title>
					<Text size="2">{t('agent_version.delete_confirm', { version: version.version })}</Text>
					<Flex gap="2" justify="end" className="mt-4">
						<Dialog.Close>
							<Button variant="soft" color="gray">
								{t('common.cancel')}
							</Button>
						</Dialog.Close>
						<Button color="red" onClick={handleDelete} disabled={actingId === version.id}>
							{t('common.delete')}
						</Button>
					</Flex>
				</Dialog.Content>
			</Dialog.Root>

			<Dialog.Root open={uploadOpen} onOpenChange={setUploadOpen}>
				<Dialog.Content style={{ maxWidth: 500 }}>
					<UploadPackageDialog versionId={version.id} onSuccess={onUpdate} onClose={() => setUploadOpen(false)} />
				</Dialog.Content>
			</Dialog.Root>
		</>
	)
}

const EditVersionDialog = ({ version, onSuccess, onClose }: { version: AgentVersion; onSuccess: () => void; onClose: () => void }) => {
	const { t } = useTranslation()
	const [submitting, setSubmitting] = useState(false)
	const [formVersion, setFormVersion] = useState(version.version)
	const [formChangelog, setFormChangelog] = useState(version.changelog || '')
	const [formIsCurrent, setFormIsCurrent] = useState(version.is_current)

	const handleEdit = async (e: React.FormEvent<HTMLFormElement>) => {
		e.preventDefault()
		if (!formVersion.trim()) {
			toast.error(t('agent_version.version_required'))
			return
		}
		setSubmitting(true)
		try {
			const resp = await fetch(`/api/admin/agent-version/${version.id}/metadata`, {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({
					version: formVersion.trim(),
					changelog: formChangelog,
					is_current: formIsCurrent
				})
			})
			if (!resp.ok) {
				throw new Error(await extractError(resp))
			}
			toast.success(t('common.updated_successfully'))
			onSuccess()
			onClose()
		} catch (error: any) {
			toast.error(t('agent_version.operation_failed') + ': ' + (error?.message || error))
		} finally {
			setSubmitting(false)
		}
	}

	return (
		<form onSubmit={handleEdit} className="flex flex-col gap-4">
			<Dialog.Title>{t('common.edit')}</Dialog.Title>

			<label className="flex flex-col gap-2">
				<Text size="2" weight="medium">
					{t('agent_version.version')} <Text color="red">*</Text>
				</Text>
				<TextField.Root placeholder="1.2.3" value={formVersion} onChange={e => setFormVersion(e.target.value)} required />
			</label>

			<label className="flex flex-col gap-2">
				<Text size="2" weight="medium">
					{t('agent_version.changelog')} <Text size="1" color="gray">({t('common.optional', '可选')})</Text>
				</Text>
				<TextArea placeholder={t('agent_version.changelog_placeholder')} value={formChangelog} onChange={e => setFormChangelog(e.target.value)} rows={4} />
			</label>

			<Flex align="center" justify="between" className="p-3 rounded-lg" style={{ backgroundColor: 'var(--accent-3)' }}>
				<div>
					<Text size="2" weight="medium">
						{t('agent_version.is_current')}
					</Text>
					<Text size="1" color="gray" className="block mt-1">
						{t('agent_version.is_current_desc')}
					</Text>
				</div>
				<Switch checked={formIsCurrent} onCheckedChange={setFormIsCurrent} />
			</Flex>

			<Flex gap="2" justify="end" className="mt-2">
				<Dialog.Close>
					<Button type="button" variant="soft" color="gray">
						{t('common.cancel')}
					</Button>
				</Dialog.Close>
				<Button type="submit" disabled={submitting}>
					{t('common.save')}
				</Button>
			</Flex>
		</form>
	)
}

const UploadPackageDialog = ({ versionId, onSuccess, onClose }: { versionId: number; onSuccess: () => void; onClose: () => void }) => {
	const { t } = useTranslation()
	const [uploading, setUploading] = useState(false)
	const [formFiles, setFormFiles] = useState<File[]>([])
	const fileInputRef = useRef<HTMLInputElement | null>(null)

	const handleUpload = async (e: React.FormEvent<HTMLFormElement>) => {
		e.preventDefault()
		if (formFiles.length === 0) {
			toast.error(t('agent_version.files_required'))
			return
		}
		setUploading(true)
		const fd = new FormData()
		formFiles.forEach(file => fd.append('files', file))
		try {
			const resp = await fetch(`/api/admin/agent-version/${versionId}/upload`, {
				method: 'POST',
				body: fd
			})
			if (!resp.ok) throw new Error(await extractError(resp))
			toast.success(t('agent_version.toast_upload'))
			onSuccess()
			onClose()
		} catch (error: any) {
			toast.error(t('agent_version.operation_failed') + ': ' + (error?.message || error))
		} finally {
			setUploading(false)
		}
	}

	return (
		<form onSubmit={handleUpload} className="flex flex-col gap-4">
			<Dialog.Title>{t('agent_version.upload_more')}</Dialog.Title>

			<label className="flex flex-col gap-2">
				<Flex justify="between" align="center">
					<Text size="2" weight="medium">
						{t('agent_version.files')}
					</Text>
					<Text size="1" color="gray">
						{t('agent_version.upload_hint')}
					</Text>
				</Flex>
				<input
					ref={fileInputRef}
					type="file"
					multiple
					onChange={e => setFormFiles(Array.from(e.target.files || []))}
					className="block w-full cursor-pointer rounded-lg border border-[var(--accent-6)] bg-[var(--accent-2)] px-3 py-2 text-sm text-[var(--accent-12)] file:mr-3 file:rounded-md file:border-0 file:bg-[var(--accent-9)] file:px-3 file:py-1 file:text-white hover:file:bg-[var(--accent-10)]"
				/>
				{formFiles.length > 0 && (
					<Flex gap="2" wrap="wrap">
						{formFiles.map(file => (
							<Badge key={file.name} color="blue">
								{file.name}
							</Badge>
						))}
					</Flex>
				)}
			</label>

			<Flex gap="2" justify="end" className="mt-2">
				<Dialog.Close>
					<Button type="button" variant="soft" color="gray">
						{t('common.cancel')}
					</Button>
				</Dialog.Close>
				<Button type="submit" disabled={uploading}>
					{uploading ? t('agent_version.uploading') : t('agent_version.submit')}
				</Button>
			</Flex>
		</form>
	)
}

const PackageTile = ({ pkg, versionId, onUpdate }: { pkg: AgentPackage; versionId: number; onUpdate: () => void }) => {
	const { t } = useTranslation()
	const [deleteOpen, setDeleteOpen] = useState(false)
	const [deleting, setDeleting] = useState(false)

	const handleDelete = async () => {
		setDeleting(true)
		try {
			const resp = await fetch(`/api/admin/agent-version/${versionId}/package/${pkg.id}`, {
				method: 'DELETE'
			})
			if (!resp.ok) throw new Error(await extractError(resp))
			toast.success(t('agent_version.toast_package_deleted'))
			setDeleteOpen(false)
			onUpdate()
		} catch (error: any) {
			toast.error(t('agent_version.operation_failed') + ': ' + (error?.message || error))
		} finally {
			setDeleting(false)
		}
	}

	const handleDownload = () => {
		window.open(`/api/admin/agent-version/${versionId}/package/${pkg.id}/download`, '_blank')
	}

	return (
		<Dialog.Root open={deleteOpen} onOpenChange={setDeleteOpen}>
			<div className="rounded-md border border-[var(--gray-a4)] bg-[var(--gray-a2)] px-3 py-2" title={pkg.file_name}>
				<Flex justify="between" align="center" gap="2">
					<div className="min-w-0">
						<Text size="2" weight="medium">
							{pkg.os} <Text color="gray">/</Text> {pkg.arch}
						</Text>
						<Text size="1" color="gray" className="block mt-1 truncate font-mono">
							{pkg.file_name}
						</Text>
					</div>
					<Flex gap="2" align="center" className="shrink-0">
						<Text size="1" color="gray">
							{formatBytes(pkg.file_size)}
						</Text>
						<IconButton
							size="1"
							variant="soft"
							color="blue"
							onClick={e => {
								e.stopPropagation()
								handleDownload()
							}}
							title={t('agent_version.download') as string}>
							<Download size={14} />
						</IconButton>
						<Dialog.Trigger>
							<IconButton size="1" variant="soft" color="red" onClick={e => e.stopPropagation()} title={t('common.delete') as string}>
								<Trash size={14} />
							</IconButton>
						</Dialog.Trigger>
					</Flex>
				</Flex>
			</div>

			<Dialog.Content style={{ maxWidth: 450 }}>
				<Dialog.Title>{t('common.delete')}</Dialog.Title>
				<Dialog.Description>
					{t('agent_version.delete_package_confirm', {
						os: pkg.os,
						arch: pkg.arch
					})}
				</Dialog.Description>
				<Flex gap="2" justify="end" className="mt-4">
					<Dialog.Close>
						<Button variant="soft" color="gray">
							{t('common.cancel')}
						</Button>
					</Dialog.Close>
					<Button color="red" onClick={handleDelete} disabled={deleting}>
						{deleting ? t('common.deleting', '删除中...') : t('common.delete')}
					</Button>
				</Flex>
			</Dialog.Content>
		</Dialog.Root>
	)
}
