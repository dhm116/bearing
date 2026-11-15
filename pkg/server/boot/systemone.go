package boot

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	jev "github.com/mattn/go-jev"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// SystemOneSettings names the System One decision model the server asks
// (ADR 7's matchers, ADR 13's trails): hosted Jev by default, or any server
// that speaks POST /v1/systemone, such as a self-hosted Kev or OpenJev.
// Absent, no model is asked.
type SystemOneSettings struct {
	// URL overrides the endpoint: a full URL, or host:port, which takes
	// /v1/systemone. Default: jev.DefaultURL, hosted Jev.
	URL string `json:"url,omitempty"`
	// Model is the model to ask. Default: jev.DefaultModel.
	Model string `json:"model,omitempty"`
	// APIKeyEnv names the environment variable that holds the API key, sent
	// as a bearer token. Empty sends none, for a local server.
	APIKeyEnv string `json:"api_key_env,omitempty"`
}

// endpoint is the URL the client posts to.
func (o *SystemOneSettings) endpoint() string {
	if o.URL == "" {
		return jev.DefaultURL
	}
	return jev.Endpoint(o.URL)
}

// check refuses a URL that is not http or https, a key variable that is not
// a variable name, and a key sent in the clear to anything but this host.
func (o *SystemOneSettings) check() error {
	if o == nil {
		return nil
	}
	u, err := url.Parse(o.endpoint())
	switch {
	case err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http"):
		return errors.New("boot: settings: system_one.url must be an http or https URL, or host:port")
	case u.User != nil:
		// Not echoed: credentials in a URL are a secret written in the settings.
		return errors.New("boot: settings: system_one.url must not hold credentials; name them with system_one.api_key_env")
	case o.APIKeyEnv != "" && !envName.MatchString(o.APIKeyEnv):
		return errors.New("boot: settings: system_one.api_key_env must be the name of an environment variable, not the key")
	case o.APIKeyEnv != "" && u.Scheme == "http" && !loopback(u.Hostname()):
		return fmt.Errorf("boot: settings: system_one.url %s would send the API key unencrypted; use https", u.Redacted())
	}
	return nil
}

// loopback reports whether host is this machine.
func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// SystemOneClient returns the client the settings name, or nil when
// system_one is absent. getenv reads the API key; an api_key_env that is
// unset or empty is an error naming the variable, never a request without
// the key. HTTP goes through otelhttp.
func (s *Settings) SystemOneClient(getenv func(string) string) (*jev.Client, error) {
	o := s.SystemOne
	if o == nil {
		return nil, nil
	}
	if err := o.check(); err != nil {
		return nil, err
	}
	opts := []jev.ClientOption{
		jev.WithURL(o.endpoint()),
		jev.WithHTTPClient(&http.Client{Timeout: 30 * time.Second, Transport: otelhttp.NewTransport(http.DefaultTransport)}),
	}
	if o.Model != "" {
		opts = append(opts, jev.WithModel(o.Model))
	}
	if o.APIKeyEnv != "" {
		key := getenv(o.APIKeyEnv)
		if key == "" {
			return nil, fmt.Errorf("boot: system_one: %s is not set", o.APIKeyEnv)
		}
		opts = append(opts, jev.WithAPIKey(key))
	}
	return jev.NewClient(opts...), nil
}
