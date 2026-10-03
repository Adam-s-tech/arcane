import { redirect } from '@sveltejs/kit';

import { authService } from '#lib/services/auth-service.js';
import { tryCatch } from '#lib/utils/try-catch.js';

import type { PageLoad } from './$types';

export const load: PageLoad = async ({ fetch, parent }) => {
	const { queryClient } = await parent();

	const operationResult = await tryCatch(
		(async () => {
			await fetch('/api/auth/logout', {
				method: 'POST',
				credentials: 'include'
			});
		})()
	);
	if (operationResult.error !== null) {
		const error = operationResult.error;

		console.error('Logout error:', error);
	}

	authService.logout(queryClient);

	throw redirect(302, '/login');
};
