package t3

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Grant is the token file's whole contents: the access token, what renews it,
// and the client and endpoint that renew it, so a refresh needs no discovery.
type Grant struct {
	URL           string    `json:"url"`
	ClientID      string    `json:"client_id"`
	TokenEndpoint string    `json:"token_endpoint"`
	AccessToken   string    `json:"access_token"`
	RefreshToken  string    `json:"refresh_token,omitempty"`
	TokenType     string    `json:"token_type,omitempty"`
	Scope         string    `json:"scope,omitempty"`
	ExpiresAt     time.Time `json:"expires_at,omitzero"`
}

// expirySlack renews a token this long before the server would refuse it, so a
// call made on the boundary does not meet a 401 mid-flight.
const expirySlack = time.Minute

// Expired reports a grant whose access token the server will refuse at now.
// A grant with no stated expiry is taken at its word until a 401 says otherwise.
func (g Grant) Expired(now time.Time) bool {
	return !g.ExpiresAt.IsZero() && !now.Add(expirySlack).Before(g.ExpiresAt)
}

// ReadGrant reads a token file.
func ReadGrant(path string) (Grant, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- the [t3] token_file the config names
	if errors.Is(err, fs.ErrNotExist) {
		return Grant{}, fmt.Errorf("no T3 Code grant at %s: run `drover t3 login`", path)
	}
	if err != nil {
		return Grant{}, err
	}
	var g Grant
	if err := json.Unmarshal(b, &g); err != nil || g.AccessToken == "" {
		return Grant{}, fmt.Errorf("%s holds no access token drover can read: run `drover t3 login` to replace it", path)
	}
	return g, nil
}

// WriteGrant writes a token file at mode 0600, through a rename so a reader
// never sees half of one.
func WriteGrant(path string, g Grant) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".t3-token-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Tokens hands out the access token from a token file, renewing it through the
// refresh token when it has expired and writing the renewed grant back.
type Tokens struct {
	Path string
	HTTP *http.Client

	mu    sync.Mutex
	grant *Grant
}

// Token is the current access token, renewed first when it has expired.
func (t *Tokens) Token(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.grant == nil {
		g, err := ReadGrant(t.Path)
		if err != nil {
			return "", err
		}
		t.grant = &g
	}
	if !t.grant.Expired(time.Now()) {
		return t.grant.AccessToken, nil
	}
	if t.grant.RefreshToken == "" || t.grant.TokenEndpoint == "" {
		return "", fmt.Errorf("the T3 Code grant in %s expired at %s and carries no refresh token: run `drover t3 login`",
			t.Path, t.grant.ExpiresAt.Format(time.RFC3339))
	}
	next, err := refresh(ctx, t.client(), *t.grant)
	if err != nil {
		return "", fmt.Errorf("the T3 Code grant in %s expired and would not renew (%v): run `drover t3 login`", t.Path, err)
	}
	if err := WriteGrant(t.Path, next); err != nil {
		return "", fmt.Errorf("renewed the T3 Code grant and could not write it to %s: %w", t.Path, err)
	}
	t.grant = &next
	return next.AccessToken, nil
}

func (t *Tokens) client() *http.Client {
	if t.HTTP != nil {
		return t.HTTP
	}
	return http.DefaultClient
}

// Metadata is what discovery found: the authorization server's endpoints, and
// the scopes the resource asks for.
type Metadata struct {
	Issuer        string   `json:"issuer"`
	Authorize     string   `json:"authorization_endpoint"`
	Token         string   `json:"token_endpoint"`
	Registration  string   `json:"registration_endpoint"`
	Methods       []string `json:"code_challenge_methods_supported"`
	ScopesSupport []string `json:"scopes_supported"`
}

// Discover finds the authorization server behind an MCP endpoint: the
// protected-resource document (RFC 9728) names it, and its own metadata
// (RFC 8414) names the endpoints. A resource publishing no document is taken
// to be its own authorization server, which is what MCP's earlier revision
// specified.
func Discover(ctx context.Context, hc *http.Client, mcpURL string) (Metadata, []string, error) {
	u, err := url.Parse(mcpURL)
	if err != nil {
		return Metadata{}, nil, err
	}
	origin := u.Scheme + "://" + u.Host
	issuer := origin
	var scopes []string
	var pr struct {
		Servers []string `json:"authorization_servers"`
		Scopes  []string `json:"scopes_supported"`
	}
	for _, p := range wellKnown("oauth-protected-resource", u.Path) {
		if getJSON(ctx, hc, origin+p, &pr) == nil {
			if len(pr.Servers) > 0 {
				issuer = strings.TrimRight(pr.Servers[0], "/")
			}
			scopes = pr.Scopes
			break
		}
	}
	iu, err := url.Parse(issuer)
	if err != nil {
		return Metadata{}, nil, fmt.Errorf("authorization server %q: %w", issuer, err)
	}
	iorigin := iu.Scheme + "://" + iu.Host
	var md Metadata
	var tried []string
	for _, p := range append(wellKnown("oauth-authorization-server", iu.Path), wellKnown("openid-configuration", iu.Path)...) {
		tried = append(tried, iorigin+p)
		if getJSON(ctx, hc, iorigin+p, &md) == nil && md.Authorize != "" && md.Token != "" {
			if len(scopes) == 0 {
				scopes = md.ScopesSupport
			}
			return md, scopes, nil
		}
	}
	return Metadata{}, nil, fmt.Errorf("found no OAuth authorization server metadata for %s (tried %s)", mcpURL, strings.Join(tried, ", "))
}

// wellKnown is the two places a metadata document can sit for a resource with
// a path: inserted before the path, and at the origin's root.
func wellKnown(name, path string) []string {
	path = strings.TrimRight(path, "/")
	if path == "" {
		return []string{"/.well-known/" + name}
	}
	return []string{"/.well-known/" + name + path, "/.well-known/" + name}
}

func getJSON(ctx context.Context, hc *http.Client, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", u, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// LoginOptions are the parts of a login a caller decides.
type LoginOptions struct {
	URL  string
	HTTP *http.Client
	// Open hands the authorization URL to a browser, and reports false when it
	// could not, in which case the URL is printed for a person to open.
	Open func(string) bool
	// Out is where the URL and progress go.
	Out io.Writer
	// Listen is the loopback address the redirect lands on; 127.0.0.1:0 picks
	// a free port.
	Listen string
}

// Login runs the authorization-code flow with PKCE against the server's
// discovered metadata and returns the grant. The redirect lands on a listener
// on the loopback interface, which is the only redirect a native client can
// receive without a hosted page of its own.
func Login(ctx context.Context, o LoginOptions) (Grant, error) {
	hc := o.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	md, scopes, err := Discover(ctx, hc, o.URL)
	if err != nil {
		return Grant{}, err
	}
	listen := o.Listen
	if listen == "" {
		listen = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return Grant{}, fmt.Errorf("listen for the sign-in redirect: %w", err)
	}
	defer func() { _ = ln.Close() }()
	redirect := "http://" + ln.Addr().String() + "/callback"

	clientID, err := register(ctx, hc, md, redirect)
	if err != nil {
		return Grant{}, err
	}
	verifier := randomString(48)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	state := randomString(24)

	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirect},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
		"resource":              {o.URL},
	}
	if len(scopes) > 0 {
		q.Set("scope", strings.Join(scopes, " "))
	}
	authURL := md.Authorize + "?" + q.Encode()

	type callback struct {
		code string
		err  error
	}
	got := make(chan callback, 1)
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		v := r.URL.Query()
		var cb callback
		switch {
		case v.Get("error") != "":
			cb.err = fmt.Errorf("the server refused the sign-in: %s %s", v.Get("error"), v.Get("error_description"))
		case v.Get("state") != state:
			cb.err = errors.New("the sign-in redirect carried a state this login did not send, so it was not answered")
		default:
			cb.code = v.Get("code")
		}
		if cb.err != nil {
			http.Error(w, cb.err.Error(), http.StatusBadRequest)
		} else {
			_, _ = io.WriteString(w, "drover is signed in to T3 Code. You can close this tab.\n")
		}
		select {
		case got <- cb:
		default:
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	if o.Open == nil || !o.Open(authURL) {
		_, _ = fmt.Fprintf(o.Out, "Open this URL to sign drover in:\n\n  %s\n\n", authURL)
	} else {
		_, _ = fmt.Fprintf(o.Out, "Opened the sign-in page in your browser. If it did not open, visit:\n\n  %s\n\n", authURL)
	}
	_, _ = fmt.Fprintln(o.Out, "Waiting for the redirect to "+redirect+" ...")

	var cb callback
	select {
	case cb = <-got:
	case <-ctx.Done():
		return Grant{}, fmt.Errorf("sign-in abandoned: %w", ctx.Err())
	}
	if cb.err != nil {
		return Grant{}, cb.err
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {cb.code},
		"redirect_uri":  {redirect},
		"client_id":     {clientID},
		"code_verifier": {verifier},
		"resource":      {o.URL},
	}
	g, err := exchange(ctx, hc, md.Token, form)
	if err != nil {
		return Grant{}, err
	}
	g.URL, g.ClientID, g.TokenEndpoint = o.URL, clientID, md.Token
	return g, nil
}

// register is dynamic client registration (RFC 7591): T3 Code issues a client
// id per outside agent, which is what lets Settings > Connections revoke one.
func register(ctx context.Context, hc *http.Client, md Metadata, redirect string) (string, error) {
	if md.Registration == "" {
		return "", errors.New("the authorization server offers no client registration endpoint, and drover has no client id of its own to sign in with")
	}
	body, _ := json.Marshal(map[string]any{
		"client_name":                "drover",
		"redirect_uris":              []string{redirect},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, md.Registration, strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("register drover as a client: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		ClientID string `json:"client_id"`
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("register drover as a client: the server answered %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.ClientID == "" {
		return "", errors.New("register drover as a client: the server returned no client_id")
	}
	return out.ClientID, nil
}

func refresh(ctx context.Context, hc *http.Client, g Grant) (Grant, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {g.RefreshToken},
		"client_id":     {g.ClientID},
		"resource":      {g.URL},
	}
	next, err := exchange(ctx, hc, g.TokenEndpoint, form)
	if err != nil {
		return Grant{}, err
	}
	next.URL, next.ClientID, next.TokenEndpoint = g.URL, g.ClientID, g.TokenEndpoint
	if next.RefreshToken == "" {
		next.RefreshToken = g.RefreshToken
	}
	return next, nil
}

func exchange(ctx context.Context, hc *http.Client, endpoint string, form url.Values) (Grant, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Grant{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return Grant{}, fmt.Errorf("token endpoint: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var tok struct {
		Access    string `json:"access_token"`
		Refresh   string `json:"refresh_token"`
		Type      string `json:"token_type"`
		Scope     string `json:"scope"`
		ExpiresIn int64  `json:"expires_in"`
		Error     string `json:"error"`
		ErrorDesc string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil {
		return Grant{}, fmt.Errorf("token endpoint answered %s with no JSON", resp.Status)
	}
	if resp.StatusCode/100 != 2 || tok.Access == "" {
		return Grant{}, fmt.Errorf("token endpoint answered %s: %s %s", resp.Status, tok.Error, tok.ErrorDesc)
	}
	g := Grant{AccessToken: tok.Access, RefreshToken: tok.Refresh, TokenType: tok.Type, Scope: tok.Scope}
	if tok.ExpiresIn > 0 {
		g.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).UTC().Truncate(time.Second)
	}
	return g, nil
}

func randomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
