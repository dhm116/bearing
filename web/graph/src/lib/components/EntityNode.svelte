<script lang="ts">
	import { Handle, Position, type NodeProps } from '@xyflow/svelte';
	import type { NodeOf } from '#lib/graph.ts';
	import { kindLabel } from '#lib/graph.ts';
	import { kindColor, sourceColor } from '#lib/palette.ts';

	let { data, selected }: NodeProps<NodeOf> = $props();

	const e = $derived(data.entity);
	const sources = $derived([...new Set(e.records.flatMap((r) => r.sources ?? []))]);
	const key = $derived(e.records.find((r) => r.label)?.label ?? '');
	const unseen = $derived(!e.kind);
</script>

<div
	class={[
		'entity relative flex h-[76px] w-[232px] flex-col justify-center gap-1 overflow-hidden rounded-box border bg-base-100 py-2 pr-3 pl-4',
		selected ? 'border-primary ring-2 ring-primary/40' : 'border-neutral',
		unseen && 'border-dashed bg-base-200',
		data.fresh && 'fresh',
		data.changed && 'changed'
	]}
	style:--kind={kindColor(e.kind)}
>
	<span class="absolute inset-y-0 left-0 w-1" style:background="var(--kind)"></span>
	<div class="flex items-center justify-between gap-2">
		<span class="font-mono text-[10px] tracking-[0.08em] uppercase" style:color="var(--kind)">{kindLabel(e.kind)}</span>
		<span class="flex gap-1">
			{#each sources as s (s)}
				<span class="size-2 rounded-full" style:background={sourceColor(s)} title={s}></span>
			{/each}
			{#if e.records.length > 1}
				<span class="font-mono text-[10px] text-base-content/60" title="{e.records.length} records joined">×{e.records.length}</span>
			{/if}
		</span>
	</div>
	<div class="truncate font-display text-[15px] leading-tight font-medium">{e.name}</div>
	<div class="truncate font-mono text-[10px] text-base-content/55">{key}</div>
</div>

<Handle type="target" position={Position.Left} id="l-in" />
<Handle type="source" position={Position.Left} id="l-out" />
<Handle type="target" position={Position.Right} id="r-in" />
<Handle type="source" position={Position.Right} id="r-out" />

<style>
	:global(.svelte-flow__node-entity .svelte-flow__handle) {
		opacity: 0;
		pointer-events: none;
		min-width: 1px;
		min-height: 1px;
		width: 1px;
		height: 1px;
		border: 0;
	}
	.fresh {
		animation: arrive 2.4s ease-out;
	}
	.changed {
		animation: shift 2.4s ease-out;
	}
	@keyframes arrive {
		0% {
			box-shadow: 0 0 0 6px color-mix(in oklab, var(--color-success) 55%, transparent);
			transform: scale(0.92);
		}
		20% {
			transform: scale(1);
		}
		100% {
			box-shadow: 0 0 0 0 transparent;
		}
	}
	@keyframes shift {
		0% {
			box-shadow: 0 0 0 6px color-mix(in oklab, var(--color-warning) 55%, transparent);
		}
		100% {
			box-shadow: 0 0 0 0 transparent;
		}
	}
	@media (prefers-reduced-motion: reduce) {
		.fresh,
		.changed {
			animation: none;
			outline: 2px solid var(--color-success);
		}
	}
</style>
