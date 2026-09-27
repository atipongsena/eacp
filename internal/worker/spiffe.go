package worker

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"net/url"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

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

	mu       sync.Mutex
	workload *workloadapi.Client             // created at the first fetch
	cache    map[string]*spiffeAudienceCache // by audience
}

// spiffeAudienceCache holds the latest SVID of one audience. Its lock allows
// one fetch at a time; callers queued behind a fetch that failed get its
// class without a fetch of their own.
type spiffeAudienceCache struct {
	mu       sync.Mutex
	attempts atomic.Int64
	failed   string
	svid     Secret
	exp      time.Time
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
	return &spiffeClient{endpoint: e.Endpoint, id: id, now: c.now, redact: c.redact, log: c.log,
		cache: map[string]*spiffeAudienceCache{}}, nil
}

func (s *spiffeClient) audience(audience string) *spiffeAudienceCache {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.cache[audience]
	if c == nil {
		c = &spiffeAudienceCache{}
		s.cache[audience] = c
	}
	return c
}

// svid returns the cached JWT-SVID for audience while it outlives minLife,
// else the SVID of a new fetch whatever its remaining life (the caller
// judges it), with its expiry; or a failure class.
func (s *spiffeClient) svid(ctx context.Context, audience string, minLife time.Duration) (Secret, time.Time, string) {
	c := s.audience(audience)
	seen := c.attempts.Load()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.svid.v != "" && s.now().Add(minLife).Before(c.exp) {
		return c.svid, c.exp, ""
	}
	if c.failed != "" && c.attempts.Load() != seen {
		return Secret{}, time.Time{}, c.failed // the fetch this caller waited for failed
	}
	v, exp, class := s.fetch(ctx, audience)
	c.failed = class
	c.attempts.Add(1)
	if class != "" {
		s.log.WarnContext(ctx, "spiffe svid fetch failed", "audience", audience, "class", class)
		return Secret{}, time.Time{}, class
	}
	c.svid, c.exp = v, exp
	if s.redact != nil {
		s.redact.Add(v.v, exp.Add(redactAfterExpiry))
	}
	s.log.InfoContext(ctx, "spiffe svid fetched", "audience", audience,
		"expires_in", exp.Sub(s.now()).Round(time.Second))
	return v, exp, ""
}

// fetch asks the Workload API for the worker's SVID for audience. The
// signature is never checked here: only the relying party judges it.
func (s *spiffeClient) fetch(ctx context.Context, audience string) (Secret, time.Time, string) {
	ctx, cancel := context.WithTimeout(ctx, tokenRequestTimeout)
	defer cancel()
	wl, err := s.client(ctx)
	if err != nil {
		return Secret{}, time.Time{}, "spiffe_unavailable"
	}
	started := s.now()
	svid, err := wl.FetchJWTSVID(ctx, jwtsvid.Params{Audience: audience, Subject: s.id})
	if err != nil {
		st, ok := status.FromError(err)
		switch {
		case ok && st.Code() == codes.PermissionDenied:
			return Secret{}, time.Time{}, "spiffe_denied"
		case ok || ctx.Err() != nil:
			return Secret{}, time.Time{}, "spiffe_unavailable"
		default: // a response go-spiffe could not accept
			return Secret{}, time.Time{}, "spiffe_invalid"
		}
	}
	token := svid.Marshal()
	if svid.ID != s.id || len(token) > maxAssertion || !tokenPattern.MatchString(token) || !svid.Expiry.After(started) {
		return Secret{}, time.Time{}, "spiffe_invalid"
	}
	return Secret{token}, svid.Expiry, ""
}

// client returns the Workload API client, creating it at the first use; a
// creation that fails is tried again at the next fetch.
func (s *spiffeClient) client(ctx context.Context) (*workloadapi.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.workload == nil {
		c, err := workloadapi.New(ctx, workloadapi.WithAddr(s.endpoint))
		if err != nil {
			return nil, err
		}
		s.workload = c
	}
	return s.workload, nil
}

// drop forgets audience's cached SVID when it is v.
func (s *spiffeClient) drop(audience string, v Secret) {
	c := s.audience(audience)
	c.mu.Lock()
	defer c.mu.Unlock()
	if v.v != "" && c.svid.v == v.v {
		c.svid, c.exp = Secret{}, time.Time{}
	}
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
// binding's audience (value_spiffe). A failed fetch withholds the binding
// with the mint back-off; an SVID is never sent for a call it could expire
// during (ADR-019 §3d).
type spiffeValue struct {
	client   *spiffeClient
	audience string
	binding  Binding
	now      func() time.Time
	log      *slog.Logger

	mu                      sync.Mutex
	current, previous       Secret // previous: replaced or rejected, scrubbed until it expires
	currentExp, previousExp time.Time
	lifetime                time.Duration // remaining life of the last SVID fetched: a longer call cannot be served
	lifetimeExp             time.Time     // that SVID's exp: past it the lifetime says nothing and the next call fetches
	backoff                 time.Duration
	backoffUntil            time.Time
}

func (v *spiffeValue) credential(ctx context.Context, validFor time.Duration) (Secret, error) {
	v.mu.Lock()
	now := v.now()
	switch {
	case now.Before(v.backoffUntil):
		v.mu.Unlock()
		return Secret{}, ErrCredentialUnavailable
	case v.current.v != "" && now.Add(validFor).Before(v.currentExp):
		s := v.current
		v.mu.Unlock()
		return s, nil
	case v.lifetime > 0 && validFor >= v.lifetime && now.Before(v.lifetimeExp):
		v.mu.Unlock()
		return Secret{}, ErrCredentialTooShort
	}
	v.mu.Unlock()
	started := v.now()
	s, exp, class := v.client.svid(ctx, v.audience, validFor)
	v.mu.Lock()
	defer v.mu.Unlock()
	now = v.now() // after the fetch: a fetch that failed slowly still backs off
	if class == "" && exp.Before(now.Add(tokenRequestTimeout)) {
		class = "spiffe_expiring" // e.g. the agent's cached copy while its server is unreachable
	}
	if class != "" {
		if now.Before(v.backoffUntil) {
			return Secret{}, ErrCredentialUnavailable // another caller's failure already set the back-off
		}
		v.backoff = min(max(2*v.backoff, minMintBackoff), maxMintBackoff)
		v.backoffUntil = now.Add(v.backoff)
		v.log.WarnContext(ctx, "spiffe credential unavailable", "tenant", v.binding.TenantID.String(),
			"ref", v.binding.Ref, "audience", v.audience, "class", class, "retry_after", v.backoff)
		return Secret{}, ErrCredentialUnavailable
	}
	v.backoff, v.backoffUntil = 0, time.Time{}
	if s.v != v.current.v {
		if v.current.v != "" {
			v.previous, v.previousExp = v.current, v.currentExp
		}
		v.current, v.currentExp = s, exp
	}
	v.lifetime, v.lifetimeExp = exp.Sub(started), exp
	if !now.Add(validFor).Before(exp) {
		// Kept for shorter calls; this one cannot be served.
		v.log.ErrorContext(ctx, "credential lifetime shorter than the call", "tenant", v.binding.TenantID.String(),
			"ref", v.binding.Ref, "audience", v.audience, "lifetime", v.lifetime, "needed", validFor)
		return Secret{}, ErrCredentialTooShort
	}
	return s, nil
}

func (v *spiffeValue) available(now time.Time) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return !now.Before(v.backoffUntil)
}

// rejected drops the current SVID when the target refused it, so the next
// call fetches again (the agent may hand out the same one).
func (v *spiffeValue) rejected(s Secret) {
	v.mu.Lock()
	if s.v == "" || s.v != v.current.v {
		v.mu.Unlock()
		return
	}
	v.previous, v.previousExp = v.current, v.currentExp
	v.current, v.currentExp = Secret{}, time.Time{}
	v.mu.Unlock()
	// Not under v.mu: the audience lock may wait on a fetch, and Available
	// and Values need v.mu.
	v.client.drop(v.audience, s)
}

// live returns the SVIDs to scrub: each until it expires.
func (v *spiffeValue) live(now time.Time) []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	var out []string
	if v.current.v != "" && now.Before(v.currentExp) {
		out = append(out, v.current.v)
	}
	if v.previous.v != "" && now.Before(v.previousExp) {
		out = append(out, v.previous.v)
	}
	return out
}
