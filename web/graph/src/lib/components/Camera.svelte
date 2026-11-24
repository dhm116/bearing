<script lang="ts">
	// Lives inside <SvelteFlow> so it can move the viewport: to the focused
	// entity and its neighbours, or to everything when focus clears.
	import { useSvelteFlow } from '@xyflow/svelte';

	let { ids, layoutKey }: { ids: string[]; layoutKey: number } = $props();
	const flow = useSvelteFlow();

	$effect(() => {
		const nodes = ids.map((id) => ({ id }));
		void layoutKey; // refit after a relayout too
		const t = setTimeout(() => {
			flow.fitView({ nodes: nodes.length ? nodes : undefined, duration: 500, padding: nodes.length ? 0.35 : 0.06, maxZoom: 1.2, minZoom: nodes.length ? 0.3 : 0.55 });
		}, 30);
		return () => clearTimeout(t);
	});
</script>
