import { loadMergedSettingsPage } from '#lib/utils/settings-load.js';

import type { PageLoad } from './$types';

export const load: PageLoad = async ({ parent }) => {
	return loadMergedSettingsPage(parent, 'timeout settings');
};
