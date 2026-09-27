package worker

import (
	"errors"
	"log/slog"
	"net/netip"
	"net/url"
	"regexp"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"

	"eacp/internal/logging"
)

// maxSPIFFEID bounds the worker's SPIFFE ID in the secrets file.
const maxSPIFFEID = 2048

var audiencePattern = regexp.MustCompile(`^[\x21-\x7e]{1,256}$`) // printable ASCII, no spaces

// spiffeEntry is the secrets file's "spiffe" object: the SPIRE agent's
// Workload API and the identity the worker must receive (ADR-019 §3d).
type spiffeEntry struct {
	Endpoint string `json:"endpoint"`
	SPIFFEID string `json:"spiffe_id"`
}

// spiffeAudience is a value_spiffe or client_assertion_spiffe object: the
// audience the JWT-SVID is fetched for.
type spiffeAudience struct {
	Audience string `json:"audience"`
}

func (a spiffeAudience) validate() error {
	if !audiencePattern.MatchString(a.Audience) {
		return errors.New("spiffe audience must be 1-256 printable characters without spaces")
	}
	return nil
}

// spiffeClient fetches the worker's JWT-SVIDs from the Workload API. It
// never contacts the agent at load.
type spiffeClient struct {
	endpoint string
	id       spiffeid.ID
	now      func() time.Time
	redact   *logging.SecretSet
	log      *slog.Logger
}

// newSpiffeClient validates the spiffe object. Its errors never repeat a
// value from it.
func newSpiffeClient(e spiffeEntry, c loadConfig) (*spiffeClient, error) {
	if err := validSPIFFEEndpoint(e.Endpoint, c.allowPlain); err != nil {
		return nil, err
	}
	if len(e.SPIFFEID) > maxSPIFFEID {
		return nil, errors.New("spiffe spiffe_id must be at most 2048 bytes")
	}
	id, err := spiffeid.FromString(e.SPIFFEID)
	if err != nil || id.Path() == "" {
		return nil, errors.New("spiffe spiffe_id must be a SPIFFE ID with a path")
	}
	return &spiffeClient{endpoint: e.Endpoint, id: id, now: c.now, redact: c.redact, log: c.log}, nil
}

// validSPIFFEEndpoint accepts unix:// with an absolute path, or, in
// development and test only, tcp:// to a loopback IP and port: the Workload
// API authenticates its callers by process, which a TCP listener cannot do.
func validSPIFFEEndpoint(raw string, allowTCP bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return errors.New("spiffe endpoint must be unix:///<absolute path>")
	}
	switch u.Scheme {
	case "unix":
		if u.Host != "" || len(u.Path) < 2 || u.Path[0] != '/' {
			return errors.New("spiffe endpoint must be unix:///<absolute path>")
		}
		return nil
	case "tcp":
		ap, err := netip.ParseAddrPort(u.Host)
		if !allowTCP || err != nil || !ap.Addr().IsLoopback() || ap.Port() == 0 || u.Path != "" {
			return errors.New("spiffe endpoint may be tcp:// only to a loopback address in development or test")
		}
		return nil
	}
	return errors.New("spiffe endpoint must be unix:///<absolute path>")
}

// spiffeValue is a static connector credential that is a JWT-SVID for the
// binding's audience (value_spiffe).
type spiffeValue struct {
	client   *spiffeClient
	audience string
	binding  Binding
	now      func() time.Time
	log      *slog.Logger
}
