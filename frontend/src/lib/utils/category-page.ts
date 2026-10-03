import type { NormalizedCategory } from '#lib/components/category-index-page.types.js';
import type { IconType } from '#lib/icons/index.js';
type CategoryLike = {
	title: string;
	url: string;
};

export function orderCategoriesByNav<T extends CategoryLike>(categories: T[], navUrls: string[]): T[] {
	const categoriesByUrl = new Map(categories.map((category) => [category.url, category]));
	const orderedCategories = navUrls.map((url) => categoriesByUrl.get(url)).filter((category): category is T => Boolean(category));
	const unmatchedCategories = categories
		.filter((category) => !navUrls.includes(category.url))
		.sort((a, b) => a.title.localeCompare(b.title));

	return [...orderedCategories, ...unmatchedCategories];
}

function getCategoryIcon<T>(iconMap: Record<string, T>, iconName: string | null | undefined, fallback: T): T {
	return iconMap[iconName ?? ''] ?? fallback;
}

export function normalizeCategory(
	category: { id: string; title: string; description: string; icon: string; url: string },
	messages: { title: () => string; description: () => string } | undefined,
	iconMap: Record<string, IconType>,
	fallbackIcon: IconType
): NormalizedCategory {
	return {
		id: category.id,
		title: messages?.title() ?? category.title,
		description: messages?.description() ?? category.description,
		icon: getCategoryIcon(iconMap, category.icon, fallbackIcon),
		href: category.url
	};
}
