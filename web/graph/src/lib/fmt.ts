// One way to tell time across the page: the viewer's local clock, with its
// zone named once (in the header and the scrubber). The demo's story date
// is a different thing and is always labelled as such.

type T = number | string | Date;

export const zone =
	new Intl.DateTimeFormat(undefined, { timeZoneName: 'short' }).formatToParts(new Date()).find((p) => p.type === 'timeZoneName')
		?.value ?? '';

/** clock is a time of day to the second, for things that happened today. */
export const clock = (t: T) => new Date(t).toLocaleTimeString(undefined, { hour: 'numeric', minute: '2-digit', second: '2-digit' });

/** when is a date and time to the minute. */
export const when = (t: T) =>
	new Date(t).toLocaleString(undefined, { month: 'short', day: 'numeric', year: 'numeric', hour: 'numeric', minute: '2-digit' });

/** day is a calendar date, for the demo's story dates (kept in UTC, as the fakes set them). */
export const day = (t: T) => new Date(t).toLocaleDateString(undefined, { month: 'short', day: 'numeric', timeZone: 'UTC' });

/** pct shows a 0–1 score as a whole percentage. */
export const pct = (x: number) => `${Math.round(x * 100)}%`;

/** ago says how long before now, in the largest sensible unit. */
export function ago(t: T, now = Date.now()) {
	const s = Math.round((now - new Date(t).getTime()) / 1000);
	if (s < 60) return `${s}s ago`;
	if (s < 3600) return `${Math.round(s / 60)} min ago`;
	return `${(s / 3600).toFixed(1)} h ago`;
}
