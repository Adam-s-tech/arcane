<script lang="ts">
	import type { HTMLAttributes } from 'svelte/elements';

	import { cn } from '#lib/utils.js';

	let { class: className, children, ...restProps }: HTMLAttributes<HTMLSpanElement> = $props();
</script>

<span
	class={cn(
		"flex items-center gap-2 text-sm text-muted-foreground [&_svg]:pointer-events-none [&_svg:not([class*='size-'])]:size-4",
		className
	)}
	{...restProps}
>
	{@render children?.()}
</span>
