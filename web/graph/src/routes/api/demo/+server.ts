// The demo's story: which steps exist, and playing one (then asking every
// source to sync, so the graph moves without waiting for the schedule).
import { json, error } from '@sveltejs/kit';
import { advance, sources, story, sync } from '#lib/server/bearing.ts';

export async function GET() {
	try {
		return json(await story());
	} catch (e) {
		error(502, e instanceof Error ? e.message : String(e));
	}
}

export async function POST({ request }) {
	const body = (await request.json().catch(() => ({}))) as { to?: string; sync?: boolean };
	try {
		const played = body.to ? await advance(body.to) : null;
		if (body.sync !== false) await Promise.all(sources().map(sync));
		return json({ played });
	} catch (e) {
		error(502, e instanceof Error ? e.message : String(e));
	}
}
