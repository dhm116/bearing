// Every error SvelteKit catches in the browser, and any the page throws
// outside it, goes to the UI log as well as the console.
import type { HandleClientError } from '@sveltejs/kit/hooks';
import { reportError } from '#lib/report.ts';

export const handleError: HandleClientError = (input) => {
	reportError('error' in input ? input.error : input, 'sveltekit');
};

addEventListener('error', (e) => reportError(e.error ?? e.message, 'window'));
addEventListener('unhandledrejection', (e) => reportError(e.reason, 'promise'));
