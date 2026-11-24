// Colours for kinds and sources. Sources are config names, so they hash to a
// hue rather than being listed by vendor.

const kindHue: Record<string, number> = {
	Person: 200,
	Team: 285,
	Repository: 60,
	Component: 150,
	Package: 25,
	Environment: 235,
	CloudResource: 320
};

export function kindColor(kind: string) {
	if (!kind) return 'light-dark(oklch(50% 0.01 220), oklch(62% 0.01 220))';
	const h = kindHue[kind] ?? hash(kind) % 360;
	return `light-dark(oklch(50% 0.13 ${h}), oklch(74% 0.11 ${h}))`;
}

export function sourceColor(source: string) {
	const h = (hash(source) * 47) % 360;
	return `light-dark(oklch(52% 0.12 ${h}), oklch(70% 0.09 ${h}))`;
}

function hash(s: string) {
	let h = 0;
	for (const c of s) h = (h * 31 + c.charCodeAt(0)) >>> 0;
	return h;
}
