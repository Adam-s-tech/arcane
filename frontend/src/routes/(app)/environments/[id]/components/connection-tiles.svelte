<script module lang="ts">
	import { Badge, type BadgeVariant } from '#lib/components/ui/badge/index.js';
	import { cn } from '#lib/utils.js';
	export { badgeTile, tile };
</script>

{#snippet badgeTile(label: string, text: string, variant: BadgeVariant)}
	<div class="flex flex-col gap-1.5 rounded-lg border border-border/50 bg-card/30 p-3">
		<div class="text-xs font-semibold tracking-wide text-muted-foreground uppercase">{label}</div>
		<div><Badge {variant} minWidth="20">{text}</Badge></div>
	</div>
{/snippet}

{#snippet tile(label: string, value: string, opts?: { mono?: boolean; subtext?: string })}
	<div class="flex flex-col gap-1 rounded-lg border border-border/50 bg-card/30 p-3">
		<div class="text-xs font-semibold tracking-wide text-muted-foreground uppercase">{label}</div>
		<div class={cn('text-sm font-medium text-foreground', opts?.mono && 'font-mono break-all select-all')}>
			{value}
		</div>
		{#if opts?.subtext}
			<div class="text-xs text-muted-foreground">{opts.subtext}</div>
		{/if}
	</div>
{/snippet}
