package store

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"bearing.example/internal/pgstore"
	"bearing.example/pkg/telemetry"
)

// postgresParams are the query parameters a postgres:// URL accepts. The
// driver reads many more (and the environment and password files besides),
// so Bearing names what it allows and refuses the rest (C-STORE-1).
var postgresParams = []string{
	"sslmode", "sslrootcert", "sslcert", "sslkey", "connect_timeout",
	"application_name", "schema", "pool_max_conns", "host",
	"insecure_store_plaintext", "insecure_store_superuser", "vector_dimensions",
}

// secretParams are parameters that carry or point to a secret; the message
// for them says where the secret goes instead.
var secretParams = []string{"password", "passfile", "service", "servicefile", "sslpassword"}

var sslModes = []string{"disable", "allow", "prefer", "require", "verify-ca", "verify-full"}

// postgresOptions turns a postgres:// or postgresql:// URL into the options
// of pgstore. The password comes from getenv(PasswordEnv) only. Connections
// to a host other than the local machine require sslmode=verify-full unless
// the URL says insecure_store_plaintext=true (C-STORE-6).
func postgresOptions(u *url.URL, getenv func(string) string) (pgstore.Options, error) {
	name := safeName(u)
	fail := func(format string, args ...any) (pgstore.Options, error) {
		return pgstore.Options{}, fmt.Errorf("store: %s URL: %s", name, fmt.Sprintf(format, args...))
	}
	q := u.Query()
	for key, vals := range q {
		switch {
		case slices.Contains(secretParams, key):
			return fail("%s is not accepted; the password is read from %s", key, PasswordEnv)
		case !slices.Contains(postgresParams, key):
			return fail("unknown parameter %q; accepted: %s", key, strings.Join(postgresParams, ", "))
		case len(vals) != 1:
			return fail("parameter %s is given more than once", key)
		}
	}
	if u.User.Username() == "" {
		return fail("names no user")
	}
	o := pgstore.Options{
		Host: u.Hostname(), Database: strings.TrimPrefix(u.Path, "/"), User: u.User.Username(),
		SSLMode: q.Get("sslmode"), SSLRootCert: q.Get("sslrootcert"), SSLCert: q.Get("sslcert"), SSLKey: q.Get("sslkey"),
		Schema: q.Get("schema"), ApplicationName: q.Get("application_name"), Getenv: getenv,
	}
	if o.Host != "" && q.Has("host") {
		return fail("give the server as the URL's host or as ?host=, not both")
	}
	if strings.Contains(u.Host, ",") || strings.Contains(q.Get("host"), ",") {
		return fail("names more than one server; give one host")
	}
	if o.Host == "" {
		o.Host = q.Get("host")
		if !strings.HasPrefix(o.Host, "/") {
			return fail("names no server host; for a Unix socket give ?host=/path/to/its/directory")
		}
	}
	if o.Database == "" || strings.Contains(o.Database, "/") {
		return fail("names no database, or one with a '/' in it; the path is the database name")
	}
	if p := u.Port(); p != "" {
		n, err := strconv.ParseUint(p, 10, 16)
		if err != nil || n == 0 {
			return fail("port is not a port number")
		}
		o.Port = uint16(n)
	}
	local := strings.HasPrefix(o.Host, "/") || isLoopback(o.Host)
	switch {
	case o.SSLMode == "" && local:
		o.SSLMode = "prefer"
	case o.SSLMode == "":
		o.SSLMode = "verify-full"
	case !slices.Contains(sslModes, o.SSLMode):
		return fail("sslmode is not one of %s", strings.Join(sslModes, ", "))
	}
	plaintext, err := boolParam(q, "insecure_store_plaintext")
	if err != nil {
		return fail("%v", err)
	}
	if !local && o.SSLMode != "verify-full" && !plaintext {
		return fail("a connection to another host needs sslmode=verify-full (C-STORE-6); set insecure_store_plaintext=true to allow anything else")
	}
	if o.AllowSuperuser, err = boolParam(q, "insecure_store_superuser"); err != nil {
		return fail("%v", err)
	}
	if v := q.Get("connect_timeout"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 600 {
			return fail("connect_timeout is a number of seconds from 1 to 600")
		}
		o.ConnectTimeout = time.Duration(n) * time.Second
	}
	if v := q.Get("pool_max_conns"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 2 || n > 100 {
			return fail("pool_max_conns is a number from 2 to 100")
		}
		o.MaxConns = int32(n) //nolint:gosec // G109: n is from 2 to 100
	}
	if v := q.Get("vector_dimensions"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > pgstore.MaxVectorDimensions {
			return fail("vector_dimensions is a number from 1 to %d", pgstore.MaxVectorDimensions)
		}
		o.VectorDimensions = n
	}
	if len(o.ApplicationName) > 63 {
		return fail("application_name is longer than 63 bytes")
	}
	o.Password = getenv(PasswordEnv)
	return o, nil
}

// boolParam reads a parameter that is "true", "false" or absent.
func boolParam(q url.Values, key string) (bool, error) {
	switch q.Get(key) {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	}
	return false, fmt.Errorf("%s is true or false", key)
}

// isLoopback reports whether host is the local machine, by name or address.
// Other names are not resolved: a name that resolves to loopback today
// can resolve elsewhere tomorrow.
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func openPostgres(ctx context.Context, u *url.URL, getenv func(string) string) (backend, error) {
	o, err := postgresOptions(u, getenv)
	if err != nil {
		return backend{}, err
	}
	log := telemetry.Logger("bearing.example/pkg/store")
	if o.AllowSuperuser {
		log.WarnContext(ctx, "insecure_store_superuser is set: the store may connect as a role that administers the server", "store", safeName(u))
	}
	if u.Query().Get("insecure_store_plaintext") == "true" {
		log.WarnContext(ctx, "insecure_store_plaintext is set: the store connection does not verify the server's certificate", "store", safeName(u))
	}
	st, err := pgstore.Open(ctx, o)
	if err != nil {
		return backend{}, fmt.Errorf("store: %w", err)
	}
	b := backend{name: "postgresql", graph: st, audit: st.AuditLog(), events: st, close: st.Close}
	if o.VectorDimensions > 0 {
		b.vector = st
	}
	return b, nil
}
