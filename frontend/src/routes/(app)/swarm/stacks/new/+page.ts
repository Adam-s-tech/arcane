import { error } from '@sveltejs/kit';

import { swarmService } from '#lib/services/swarm-service.js';
import { templateService } from '#lib/services/template-service.js';
import { variableService } from '#lib/services/variable-service.js';
import { tryCatch } from '#lib/utils/try-catch.js';

import type { PageLoad } from './$types';

export const load: PageLoad = async ({ url }) => {
	const templateId = url.searchParams.get('templateId');
	const fromStack = url.searchParams.get('fromStack');
	const sourceStackName = fromStack ? decodeURIComponent(fromStack) : null;
	const isEditMode = sourceStackName !== null;

	const sourceStackPromise = sourceStackName
		? tryCatch(swarmService.getStackSource(sourceStackName)).then((result) => {
				if (result.error) {
					const err: any = result.error;

					console.warn('Failed to load source stack content:', err);
					if (err?.status === 404) {
						throw error(404, 'Saved source not found');
					}
					throw error(err?.status || 500, err?.message || 'Failed to load saved stack source');
				}
				return result.data;
			})
		: Promise.resolve(null);

	const [allTemplates, defaultTemplates, selectedTemplate, sourceStack, globalVariables] = await Promise.all([
		tryCatch(templateService.getAllTemplates()).then((result) => {
			if (result.error) {
				const err: Error = result.error;

				console.warn('Failed to load templates:', err);
				return [];
			}
			return result.data;
		}),
		tryCatch(templateService.getDefaultTemplates()).then((result) => {
			if (result.error) {
				const err: Error = result.error;

				console.warn('Failed to load default templates:', err);
				return { composeTemplate: '', swarmStackTemplate: '', swarmStackEnvTemplate: '', envTemplate: '' };
			}
			return result.data;
		}),
		templateId
			? tryCatch(templateService.getTemplateContent(templateId)).then((result) => {
					if (result.error) {
						const err: Error = result.error;

						console.warn('Failed to load selected template:', err);
						return null;
					}
					return result.data;
				})
			: Promise.resolve(null),
		sourceStackPromise,
		tryCatch(variableService.list()).then((result) => {
			if (result.error) {
				const err: Error = result.error;

				console.warn('Failed to load global variables:', err);
				return [];
			}
			return result.data;
		})
	]);

	return {
		composeTemplates: allTemplates,
		envTemplate: isEditMode
			? (sourceStack?.envContent ?? '')
			: (selectedTemplate?.envContent ?? defaultTemplates.swarmStackEnvTemplate),
		defaultTemplate: isEditMode
			? (sourceStack?.composeContent ?? '')
			: (selectedTemplate?.content ?? defaultTemplates.swarmStackTemplate),
		overrideTemplate: sourceStack?.overrideContent ?? '',
		sourceFiles: sourceStack?.files ?? [],
		isEditMode,
		selectedTemplate: selectedTemplate?.template || null,
		sourceStackName: sourceStack?.name || sourceStackName || null,
		globalVariables
	};
};
