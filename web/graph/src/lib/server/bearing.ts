// Talks to the Bearing server and the demo fakes. Runs only on the server, so
// the API token never reaches the browser.
import * as env from '$app/env/private';
import { readFileSync } from 'node:fs';
import type { View } from '#lib/types.ts';

const server = () => (env.BEARING_SERVER).replace(/\/$/, '');
const control = () => (env.BEARING_DEMO_CONTROL).replace(/\/$/, '');
export const sources = () =>
	(env.BEARING_SOURCES).split(',').filter(Boolean);

let ca: string | undefined;
function tls() {
	// Bun's fetch takes the demo CA directly; without one, the system roots apply.
	if (ca === undefined) ca = env.BEARING_CA ? readFileSync(env.BEARING_CA, 'utf8') : '';
	return ca ? { tls: { ca } } : {};
}

async function bearing(path: string, init: RequestInit = {}) {
	if (!env.BEARING_TOKEN) throw new Error('set BEARING_TOKEN to the API token');
	const res = await fetch(server() + path, {
		...init,
		...tls(),
		headers: { authorization: `Bearer ${env.BEARING_TOKEN}`, ...init.headers },
		signal: AbortSignal.timeout(10_000)
	} as RequestInit);
	if (!res.ok) throw new Error(`bearing ${path}: ${res.status} ${await res.text()}`);
	return res;
}

export async function graph(): Promise<View> {
	return (await bearing('/v1/graph')).json();
}

export async function sync(source: string) {
	await bearing(`/v1/sources/${encodeURIComponent(source)}/sync`, { method: 'POST' });
}

export type Story = { now: string; steps: { id: string; name: string; at: string; applied: boolean }[] };

export async function story(): Promise<Story> {
	const res = await fetch(control() + '/demo/steps', { signal: AbortSignal.timeout(5_000) });
	if (!res.ok) throw new Error(`demo steps: ${res.status}`);
	return res.json();
}

export async function advance(to: string) {
	const res = await fetch(`${control()}/demo/advance?to=${encodeURIComponent(to)}`, {
		method: 'POST',
		headers: { 'X-Bearing-Demo': '1' },
		signal: AbortSignal.timeout(30_000)
	});
	if (!res.ok) throw new Error(`demo advance: ${res.status} ${await res.text()}`);
	return res.json();
}
