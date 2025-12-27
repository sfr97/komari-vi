import { Dialog, Box, Flex, Text, Button } from '@radix-ui/themes'
import { useTranslation } from 'react-i18next'
import type { ForwardRule } from '..'

type Props = {
	rule: ForwardRule | null
	onClose: () => void
}

const RuleDetailDialog = ({ rule, onClose }: Props) => {
	const { t } = useTranslation()

	return (
		<Dialog.Root open={Boolean(rule)} onOpenChange={open => (!open ? onClose() : null)}>
			<Dialog.Content maxWidth="720px">
				<Dialog.Title>{rule?.name}</Dialog.Title>
				<Flex direction="column" gap="2" className="text-sm text-gray-700">
					<Text>
						{t('forward.type')}: {rule?.type}
					</Text>
					<Text>
						{t('forward.status')}: {rule?.status}
					</Text>
					<Text>
						{t('forward.group')}: {rule?.group_name || '-'}
					</Text>
					<Text>
						{t('forward.entry')}: {rule?.config_json ? '' : '-'}
					</Text>
					<Box className="bg-gray-50 border rounded p-3 overflow-auto max-h-96 whitespace-pre-wrap">
						{rule?.config_json || t('forward.configPlaceholder')}
					</Box>
				</Flex>
				<Flex justify="end" mt="4">
					<Button variant="soft" onClick={onClose}>
						{t('common.close', { defaultValue: '关闭' })}
					</Button>
				</Flex>
			</Dialog.Content>
		</Dialog.Root>
	)
}

export default RuleDetailDialog
