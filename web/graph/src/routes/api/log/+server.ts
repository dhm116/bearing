// Writes errors the browser reports to this server's log, so they show in
// web/graph/.dev.log next to the server's own. Text only, size-capped.
export async function POST({ request }) {
	const body = (await request.text()).slice(0, 10_000);
	try {
		const { where, error, url } = JSON.parse(body) as { where?: string; error?: string; url?: string };
		console.error(`[browser ${where ?? '?'}] ${url ?? ''}\n${error ?? ''}`);
	} catch {
		console.error('[browser] unreadable report');
	}
	return new Response(null, { status: 204 });
}
