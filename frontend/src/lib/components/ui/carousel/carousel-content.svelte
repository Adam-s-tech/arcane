<script lang="ts">
	import emblaCarouselSvelte from 'embla-carousel-svelte';
	import { fromAction } from 'svelte/attachments';
	import type { HTMLAttributes } from 'svelte/elements';

	import { cn, type WithElementRef } from '#lib/utils.js';

	import { getEmblaContext, type EmblaCarouselConfig } from './context.js';

	let {
		ref = $bindable(null),
		class: className,
		children,
		...restProps
	}: WithElementRef<HTMLAttributes<HTMLDivElement>> = $props();

	const emblaCtx = getEmblaContext();
</script>

<div
	data-slot="carousel-content"
	class="overflow-hidden"
	{@attach fromAction(emblaCarouselSvelte, (): EmblaCarouselConfig => ({
		options: {
			container: '[data-embla-container]',
			slides: '[data-embla-slide]',
			...emblaCtx.options,
			axis: emblaCtx.orientation === 'horizontal' ? 'x' : 'y'
		},
		plugins: emblaCtx.plugins
	}))}
	{...{ onemblaInit: emblaCtx.onInit }}
>
	<div
		bind:this={ref}
		class={cn('flex', emblaCtx.orientation === 'horizontal' ? '-ms-4' : '-mt-4 flex-col', className)}
		data-embla-container=""
		{...restProps}
	>
		{@render children?.()}
	</div>
</div>
