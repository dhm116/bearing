// The picture as Bearing knew it at ?at= (RFC 3339), for comparing with now.
import { error, json } from '@sveltejs/kit';
import { graph } from '#lib/server/bearing.ts';

export async function GET({ url }) {
	const at = url.searchParams.get('at') ?? '';
	if (at && Number.isNaN(Date.parse(at))) error(400, 'at must be an RFC 3339 time');
	try {
		return json(await graph(at || undefined));
	} catch (e) {
		error(502, e instanceof Error ? e.message : String(e));
	}
}
