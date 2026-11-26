// Sends a browser error to the UI's server, which writes it to its log
// (web/graph/.dev.log, `make demo-logs`): errors in the page are otherwise
// only in the browser console.
export function reportError(error: unknown, where: string) {
	const text = error instanceof Error ? (error.stack ?? error.message) : String(error);
	console.error(`[${where}]`, error);
	try {
		navigator.sendBeacon('/api/log', JSON.stringify({ where, error: text.slice(0, 8000), url: location.href }));
	} catch {
		// The log is best effort; the console still has it.
	}
}
