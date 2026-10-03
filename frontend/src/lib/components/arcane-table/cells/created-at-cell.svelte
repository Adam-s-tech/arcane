<script lang="ts">
	import { m } from '#lib/paraglide/messages.js';
	import { formatDateTimeShort } from '#lib/utils/formatting.js';

	let { value }: { value: unknown } = $props();
</script>

<span class="text-sm">{value ? formatDateTimeShort(String(value)) : m.common_na()}</span>
