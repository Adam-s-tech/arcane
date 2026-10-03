import { toast } from 'svelte-sonner';

import { m } from '#lib/paraglide/messages.js';
import { confirmAndRun } from '#lib/utils/bulk-actions.js';

export function confirmDeleteApiKey({
	name,
	run,
	setLoading,
	successMessage,
	onDeleted
}: {
	name: string;
	run: () => Promise<void>;
	setLoading: (loading: boolean) => void;
	successMessage: (safeName: string) => string;
	onDeleted: () => Promise<void>;
}): void {
	const safeName = name?.trim() || m.common_unknown();
	confirmAndRun({
		title: m.api_key_delete_title({ name: safeName }),
		message: m.api_key_delete_message({ name: safeName }),
		confirmLabel: m.common_delete(),
		destructive: true,
		setLoading,
		run,
		failureMessage: m.api_key_delete_failed({ name: safeName }),
		onSuccess: async () => {
			toast.success(successMessage(safeName));
			await onDeleted();
		}
	});
}
