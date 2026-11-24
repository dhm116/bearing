// Server-sent events: the whole graph whenever it changes, and the link's
// health. One upstream poll per open page.
// shortcut: polls GET /v1/graph every 1.5 s; upgrade to a server push from
// Bearing's event log when one exists.
import { graph } from '#lib/server/bearing.ts';

const every = 1500;

export function GET({ request }) {
	let timer: ReturnType<typeof setTimeout> | undefined;
	let last = '';
	let closed = false;
	const enc = new TextEncoder();

	const stream = new ReadableStream({
		start(ctl) {
			const send = (event: string, data: unknown) =>
				!closed && ctl.enqueue(enc.encode(`event: ${event}\ndata: ${JSON.stringify(data)}\n\n`));
			const tick = async () => {
				try {
					const v = await graph();
					const s = JSON.stringify(v);
					if (s !== last) {
						last = s;
						send('view', v);
					}
					send('status', { ok: true, at: new Date().toISOString() });
				} catch (e) {
					send('status', { ok: false, error: e instanceof Error ? e.message : String(e) });
				}
				if (!closed) timer = setTimeout(tick, every);
			};
			tick();
		},
		cancel() {
			closed = true;
			clearTimeout(timer);
		}
	});
	request.signal.addEventListener('abort', () => {
		closed = true;
		clearTimeout(timer);
	});

	return new Response(stream, {
		headers: { 'content-type': 'text/event-stream', 'cache-control': 'no-cache', connection: 'keep-alive' }
	});
}
