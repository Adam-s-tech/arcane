import { onDestroy } from 'svelte';

import { getSettingsFormContext, hasSettingsFormContext } from '#lib/hooks/settings-form-context.js';
import { settingsService } from '#lib/services/settings-service.js';
import settingsStore from '#lib/stores/config-store.svelte.js';
import type { SettingsFormContext, SettingsFormState } from '#lib/types/settings-form.js';
import type { Settings } from '#lib/types/settings.js';
import { tryCatch } from '#lib/utils/try-catch.js';

type SettingsPayload = Partial<Settings> & Record<string, unknown>;

type Options<TFormInputs, TSaveData extends SettingsPayload> = {
	formInputs: () => TFormInputs;
	getCurrentSettings: () => TSaveData;
	/**
	 * Custom save handler. If provided, this will be called instead of the default
	 * settingsService.updateSettings(). Useful for environment-specific settings.
	 */
	onSave?: (data: TSaveData) => Promise<void>;
};

export class UseSettingsForm<
	TFormInputs extends Record<string, { value: unknown; error: string | null }>,
	TSaveData extends SettingsPayload
> implements SettingsFormState {
	#isLoading = $state(false);
	#formValues: () => TFormInputs;
	#saveFunction: (() => Promise<void> | void) | null = null;
	#resetFunction: (() => void) | null = null;
	private formContext: SettingsFormContext | undefined;
	private getCurrentSettings: () => TSaveData;
	private customOnSave?: (data: TSaveData) => Promise<void>;

	constructor({ formInputs, getCurrentSettings, onSave }: Options<TFormInputs, TSaveData>) {
		this.getCurrentSettings = getCurrentSettings;
		this.customOnSave = onSave;
		this.#formValues = formInputs;

		this.formContext = hasSettingsFormContext() ? getSettingsFormContext() : undefined;

		if (this.formContext) {
			onDestroy(() => {
				if (this.formContext?.activeForm === this) {
					this.formContext.activeForm = undefined;
				}
			});
		}
	}

	#hasChanges = $derived.by(() => {
		const currentFormValues = this.#formValues();

		const settingsToCompare = this.getCurrentSettings();
		const keys = Object.keys(currentFormValues) as (keyof TFormInputs)[];

		return keys.some((key) => {
			const input = currentFormValues[key];
			if (input && 'value' in input) {
				return input.value !== settingsToCompare[key as string];
			}
			return false;
		});
	});

	async updateSettings(updatedSettings: Partial<TSaveData>) {
		// Use custom save handler if provided
		if (this.customOnSave) {
			const mergedSettings = {
				...this.getCurrentSettings(),
				...updatedSettings
			} as TSaveData;
			await this.customOnSave(mergedSettings);
		} else {
			const result = await tryCatch(settingsService.updateSettings(updatedSettings));

			if (result.error) {
				console.error('Error updating settings:', result.error);
				throw result.error;
			}
		}

		await settingsStore.reload();
	}

	registerFormActions(saveFunction: () => Promise<void> | void, resetFunction: () => void) {
		this.#saveFunction = saveFunction;
		this.#resetFunction = resetFunction;
		if (this.formContext) this.formContext.activeForm = this;
	}

	setLoading(loading: boolean) {
		this.#isLoading = loading;
	}

	get hasChanges() {
		return this.#hasChanges;
	}

	get isLoading() {
		return this.#isLoading;
	}

	// Read through SettingsFormState by the settings layouts.
	// fallow-ignore-next-line unused-class-member
	get saveFunction() {
		return this.#saveFunction ?? undefined;
	}

	// Read through SettingsFormState by the settings layouts.
	// fallow-ignore-next-line unused-class-member
	get resetFunction() {
		return this.#resetFunction ?? undefined;
	}
}
