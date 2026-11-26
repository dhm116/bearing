<script lang="ts">
	// Lives inside <SvelteFlow> so it can move the viewport. It frames the
	// graph once, when it first has cards, and afterwards moves only when
	// someone asks: focusing a card, or jumping to changes from the pill.
	// Updates that stream in never touch the zoom someone chose.
	import { useSvelteFlow } from '@xyflow/svelte';

	let {
		ids,
		count,
		frame
	}: { ids: string[]; count: number; frame: { ids: string[]; n: number } } = $props();
	const flow = useSvelteFlow();
	let framed = false;
	const readable = 0.55; // below this the cards' text can't be read

	$effect(() => {
		if (framed || count === 0) return;
		framed = true;
		const t = setTimeout(async () => {
			await flow.fitView({ padding: 0.06, maxZoom: 1.2 });
			// Too big to read whole: start at the top-left at a readable size
			// (people, then teams) instead of clipping both edges.
			if (flow.getViewport().zoom < readable) flow.setViewport({ x: 32, y: 32, zoom: readable });
		}, 60);
		return () => clearTimeout(t);
	});

	function fit(list: string[]) {
		if (!list.length) return;
		const nodes = list.map((id) => ({ id }));
		return setTimeout(() => flow.fitView({ nodes, duration: 500, padding: 0.35, maxZoom: 1.2, minZoom: 0.3 }), 30);
	}

	$effect(() => {
		const t = fit(ids);
		return () => clearTimeout(t);
	});

	$effect(() => {
		void frame.n;
		const t = fit(frame.ids);
		return () => clearTimeout(t);
	});
</script>
