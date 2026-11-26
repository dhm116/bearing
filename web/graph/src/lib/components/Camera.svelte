<script lang="ts">
	// Lives inside <SvelteFlow> so it can move the viewport. It frames the
	// whole graph once, when it first has cards, and afterwards moves only
	// when someone focuses a card: updates that stream in never touch the
	// zoom someone chose.
	import { useSvelteFlow } from '@xyflow/svelte';

	let { ids, count }: { ids: string[]; count: number } = $props();
	const flow = useSvelteFlow();
	let framed = false;

	$effect(() => {
		if (framed || count === 0) return;
		framed = true;
		const t = setTimeout(() => flow.fitView({ padding: 0.06, maxZoom: 1.2, minZoom: 0.55 }), 60);
		return () => clearTimeout(t);
	});

	$effect(() => {
		if (!ids.length) return;
		const nodes = ids.map((id) => ({ id }));
		const t = setTimeout(() => flow.fitView({ nodes, duration: 500, padding: 0.35, maxZoom: 1.2, minZoom: 0.3 }), 30);
		return () => clearTimeout(t);
	});
</script>
