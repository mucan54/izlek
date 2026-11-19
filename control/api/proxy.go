package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mucan54/izlek/control/config"
	"github.com/mucan54/izlek/control/db"
)

// queryProxy forwards query requests to the query service on behalf of a
// project the caller may read (spec/control.md §8, spec/query.md §1).
type queryProxy struct {
	base    string
	secret  string
	timeout time.Duration
	client  *http.Client
}

func newQueryProxy(cfg config.Config, transport http.RoundTripper) *queryProxy {
	if transport == nil {
		transport = &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: cfg.QueryTimeout,
			MaxIdleConns:          32,
			MaxIdleConnsPerHost:   32,
			IdleConnTimeout:       90 * time.Second,
			// Bodies pass through byte for byte: the transport must neither
			// ask for nor undo compression on its own.
			DisableCompression: true,
		}
	}
	return &queryProxy{
		base: cfg.QueryURL, secret: cfg.InternalSecret, timeout: cfg.QueryTimeout,
		client: &http.Client{
			Transport: transport,
			// The query service never redirects; following one could carry
			// the internal secret somewhere else.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// Query endpoints reachable with POST (spec/control.md §8).
var queryEndpoints = map[string]bool{"trends": true, "funnel": true, "retention": true, "events": true, "sql": true, "schema": true}

// Headers copied from the client request. Everything else is dropped — in
// particular every X-LA-* header (a client must not be able to choose the
// project or present the internal credential), cookies and Authorization.
var proxyRequestHeaders = []string{"Accept", "Accept-Encoding", "Content-Type"}

// Headers copied from the upstream response.
var proxyResponseHeaders = []string{"Content-Type", "Content-Encoding", "Content-Length", "Cache-Control", "Etag", "Last-Modified", "Retry-After", "Vary"}

// POST /api/projects/{pid}/q/{endpoint}
func (s *Server) proxyQuery(c *reqCtx) (any, error) {
	endpoint := c.params["endpoint"]
	if !queryEndpoints[endpoint] {
		if endpoint == "replay" {
			return nil, errCode(http.StatusMethodNotAllowed, "method_not_allowed", "Bu yöntem desteklenmiyor")
		}
		return nil, errNotFound()
	}
	p, err := c.project(db.RoleViewer)
	if err != nil {
		return nil, err
	}
	body, err := c.rawBody()
	if err != nil {
		return nil, err
	}
	return nil, s.forward(c, p.ID, http.MethodPost, "/q/"+endpoint, body)
}

// GET /api/projects/{pid}/q/replay?key=…
func (s *Server) proxyReplay(c *reqCtx) (any, error) {
	p, err := c.project(db.RoleViewer)
	if err != nil {
		return nil, err
	}
	key := c.r.URL.Query().Get("key")
	if key == "" {
		return nil, errBadRequest(`"key" zorunludur`)
	}
	// The query service enforces this too; refusing here keeps a foreign
	// key from ever leaving the control plane.
	if !strings.HasPrefix(key, fmt.Sprintf("replay/p=%d/", p.ID)) {
		return nil, errForbidden()
	}
	return nil, s.forward(c, p.ID, http.MethodGet, "/q/replay?key="+url.QueryEscape(key), nil)
}

// upstream sends one request to the query service on behalf of projectID
// and returns its response. The caller must close the body and then call
// the returned release function (the request's deadline covers reading the
// body). Failures to reach the service, and its rejection of the internal
// credential, come back as API errors.
func (s *Server) upstream(c *reqCtx, projectID uint32, method, pathAndQuery string, body []byte) (*http.Response, func(), error) {
	px := s.proxy
	if px.base == "" || px.secret == "" {
		s.mProxy.With("not_configured").Inc()
		return nil, nil, errCode(http.StatusServiceUnavailable, "query_not_configured", "Sorgu servisi yapılandırılmamış")
	}
	ctx, cancel := context.WithTimeout(c.ctx(), px.timeout)
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, px.base+pathAndQuery, rd)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	for _, h := range proxyRequestHeaders {
		if v := c.r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-LA-Internal", px.secret)
	req.Header.Set("X-LA-Project", strconv.FormatUint(uint64(projectID), 10))
	req.Header.Set("X-Request-Id", c.info.id)

	resp, err := px.client.Do(req)
	if err != nil {
		cancel()
		if c.ctx().Err() != nil { // the client went away
			s.mProxy.With("client_gone").Inc()
			return nil, nil, c.ctx().Err()
		}
		var ne net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
			s.mProxy.With("timeout").Inc()
			return nil, nil, errCode(http.StatusGatewayTimeout, "upstream_timeout", "Sorgu servisi zamanında yanıt vermedi")
		}
		s.mProxy.With("unavailable").Inc()
		s.log.Warn("query service unreachable", "req_id", c.info.id, "err", err.Error())
		return nil, nil, errCode(http.StatusBadGateway, "upstream_unavailable", "Sorgu servisine ulaşılamıyor")
	}

	// 401 from the query service is about the control plane's own
	// credential, never about the user: passing it on would make the UI
	// believe the session ended.
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		cancel()
		s.mProxy.With("upstream_auth").Inc()
		s.log.Error("query service rejected the internal credential; check LA_QUERY_INTERNAL_SECRET on both sides", "req_id", c.info.id)
		return nil, nil, errCode(http.StatusBadGateway, "upstream_auth", "Sorgu servisi iç kimlik bilgisini reddetti")
	}
	if resp.StatusCode >= 500 {
		s.mProxy.With("upstream_error").Inc()
	} else {
		s.mProxy.With("ok").Inc()
	}
	return resp, cancel, nil
}

func (s *Server) forward(c *reqCtx, projectID uint32, method, pathAndQuery string, body []byte) error {
	resp, release, err := s.upstream(c, projectID, method, pathAndQuery, body)
	if err != nil {
		return err
	}
	defer release()
	defer resp.Body.Close()

	h := c.w.Header()
	for _, name := range proxyResponseHeaders {
		if v := resp.Header.Values(name); len(v) > 0 {
			h[name] = v
		}
	}
	if h.Get("Cache-Control") == "" {
		if method == http.MethodGet && resp.StatusCode == http.StatusOK {
			h.Set("Cache-Control", "private, max-age=3600") // replay chunks are immutable
		} else {
			h.Set("Cache-Control", "no-store")
		}
	}
	_ = http.NewResponseController(c.w).SetWriteDeadline(time.Now().Add(s.proxy.timeout + 30*time.Second))
	c.w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(c.w, resp.Body); err != nil {
		// Headers are out; the only honest signal left is a broken stream.
		s.log.Warn("query proxy stream interrupted", "req_id", c.info.id, "err", err.Error())
		panic(http.ErrAbortHandler)
	}
	return nil
}
