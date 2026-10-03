import { error } from '@sveltejs/kit';

import { swarmService } from '#lib/services/swarm-service.js';
import { tryCatch } from '#lib/utils/try-catch.js';

import type { PageLoad } from './$types';

export const load: PageLoad = async ({ params }) => {
	const serviceId = params.serviceId;

	const operationResult = await tryCatch(
		(async () => {
			const service = await swarmService.getService(serviceId);

			if (!service) {
				throw error(404, 'Service not found');
			}

			return { service };
		})()
	);
	if (operationResult.error !== null) {
		const err = operationResult.error;

		console.error('Failed to load service:', err);
		if ('status' in err && err.status === 404) {
			throw err;
		}
		throw error(500, err.message || 'Failed to load service details');
	} else {
		return operationResult.data;
	}
};
