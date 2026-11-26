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
	const gone = $derived(data.mark === 'remove');
	const badge = { add: 'new', change: 'updated', remove: 'gone' } as const;
</script>

<div class="relative">
	<div
		class={[
			'entity relative flex h-[76px] w-[232px] flex-col justify-center gap-1.5 overflow-hidden rounded-box border bg-base-100 py-2 pr-3 pl-4',
			selected ? 'border-primary ring-2 ring-primary/40' : 'border-neutral',
			unseen && 'border-dashed bg-base-200',
			data.mark && `m-${data.mark}`
		]}
		style:--kind={kindColor(e.kind)}
	>
		<!-- The stripe and the kind word carry the same thing, so colour is never the only cue. -->
		<span class="absolute inset-y-0 left-0 w-1" style:background="var(--kind)"></span>
		<div class="flex items-center justify-between gap-2">
			<span class={['truncate font-display text-[15px] leading-tight font-medium', gone && 'line-through decoration-error/70']}>{e.name}</span>
			<span class="flex shrink-0 items-center gap-1" title="Read from {sources.join(', ') || 'no source yet'}">
				{#each sources as s (s)}
					<span class="size-2 rounded-full" style:background={sourceColor(s)}></span>
				{/each}
				{#if e.records.length > 1}
					<span class="text-[11px] text-base-content/70">{e.records.length} joined</span>
				{/if}
			</span>
		</div>
		<div class="flex min-w-0 items-baseline gap-2 text-[11px]">
			<span class="shrink-0 font-medium" style:color="var(--kind)">{kindLabel(e.kind)}</span>
			<span class="truncate font-mono text-base-content/70">{key}</span>
		</div>
	</div>
	{#if data.mark}
		<span
			class={[
				'badge absolute -top-2.5 right-2 h-5 border-0 px-1.5 text-[11px] font-semibold',
				data.mark === 'add' && 'bg-success text-success-content',
				data.mark === 'change' && 'bg-warning text-warning-content',
				data.mark === 'remove' && 'bg-error text-error-content'
			]}>{badge[data.mark]}</span
		>
	{/if}
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
	.m-add {
		border-color: var(--color-success);
		box-shadow: 0 0 0 1px var(--color-success);
		animation: arrive 1.6s cubic-bezier(0.16, 1, 0.3, 1);
	}
	.m-change {
		border-color: var(--color-warning);
		box-shadow: 0 0 0 1px var(--color-warning);
		animation: shift 1.6s cubic-bezier(0.16, 1, 0.3, 1);
	}
	.m-remove {
		border-style: dashed;
		border-color: var(--color-error);
		background: color-mix(in oklab, var(--color-error) 8%, var(--color-base-100));
		opacity: 0.75;
	}
	@keyframes arrive {
		0% {
			box-shadow: 0 0 0 8px color-mix(in oklab, var(--color-success) 50%, transparent);
			transform: scale(0.94);
		}
		25% {
			transform: scale(1);
		}
	}
	@keyframes shift {
		0% {
			box-shadow: 0 0 0 8px color-mix(in oklab, var(--color-warning) 50%, transparent);
		}
	}
	@media (prefers-reduced-motion: reduce) {
		.m-add,
		.m-change {
			animation: none;
		}
	}
</style>
