// The server's settings. All are private: they never reach the browser.
import { defineEnvVars } from '@sveltejs/kit/env';

const or = (fallback: string) => (v: string | undefined) => v || fallback;

export const variables = defineEnvVars({
	// The Bearing API, its token and the CA that signed its certificate.
	BEARING_SERVER: { schema: or('https://localhost:8443') },
	BEARING_TOKEN: { schema: or('') },
	BEARING_CA: { schema: or('') },
	// The demo fakes' control endpoint and the sources to sync after a step.
	BEARING_DEMO_CONTROL: { schema: or('http://127.0.0.1:9090') },
	BEARING_SOURCES: { schema: or('github-acme,directory,kubernetes-acme') }
});
