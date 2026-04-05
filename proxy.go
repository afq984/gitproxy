package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// ProxyConfig holds the proxy configuration.
type ProxyConfig struct {
	Upstream        *url.URL
	AuthType        string // "basic" or "bearer"
	Token           string
	Username        string
	ApprovalTimeout time.Duration
}

// Proxy is the Git HTTP reverse proxy.
type Proxy struct {
	config   ProxyConfig
	client   *http.Client
	approver Approver
}

// NewProxy creates a new Proxy.
func NewProxy(cfg ProxyConfig, approver Approver) *Proxy {
	return &Proxy{
		config:   cfg,
		client:   &http.Client{},
		approver: approver,
	}
}

// classify determines whether a request is a Git read, write-preflight,
// write, or unknown (non-Git). Only recognized Git endpoints are allowed.
func classify(r *http.Request) string {
	p := r.URL.Path
	query := r.URL.RawQuery

	// Smart HTTP endpoints.
	if strings.HasSuffix(p, "/info/refs") && r.Method == http.MethodGet {
		if strings.Contains(query, "service=git-receive-pack") {
			return "write-preflight"
		}
		// git-upload-pack service or dumb-protocol info/refs (no service param).
		return "read"
	}
	if strings.HasSuffix(p, "/git-upload-pack") && r.Method == http.MethodPost {
		return "read"
	}
	if strings.HasSuffix(p, "/git-receive-pack") && r.Method == http.MethodPost {
		return "write"
	}

	// Dumb HTTP protocol — GET-only object/ref endpoints.
	if r.Method == http.MethodGet {
		if strings.HasSuffix(p, "/HEAD") ||
			strings.Contains(p, "/objects/") ||
			strings.HasSuffix(p, "/info/packs") {
			return "read"
		}
	}

	return "unknown"
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	class := classify(r)
	log.Printf("%s %s [%s]", r.Method, r.URL.RequestURI(), class)

	switch class {
	case "write":
		p.handleWrite(w, r)
	case "read", "write-preflight":
		p.forward(w, r)
	default:
		http.Error(w, "forbidden: not a git endpoint", http.StatusForbidden)
	}
}

// upstreamURL constructs the full upstream URL for a given request,
// preserving any path prefix from the configured upstream.
// The request path is cleaned to prevent traversal above the upstream prefix.
func (p *Proxy) upstreamURL(r *http.Request) url.URL {
	u := *p.config.Upstream
	// Clean the request path to collapse any ".." components,
	// preventing traversal above the configured upstream prefix.
	reqPath := path.Clean("/" + r.URL.Path)
	u.Path = strings.TrimSuffix(u.Path, "/") + reqPath
	u.RawQuery = r.URL.RawQuery
	return u
}

// forward proxies a request to the upstream server.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request) {
	upstreamURL := p.upstreamURL(r)

	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL.String(), r.Body)
	if err != nil {
		log.Printf("error creating upstream request: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	copyRequestHeaders(r, upReq)
	p.injectAuth(upReq)

	resp, err := p.client.Do(upReq)
	if err != nil {
		log.Printf("upstream error: %v", err)
		http.Error(w, "upstream connection failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	relayResponse(w, resp)
}

// forwardBody proxies a request with a provided body to the upstream server.
// It returns the response for further inspection. The caller must close the body.
func (p *Proxy) forwardBody(r *http.Request, body io.Reader) (*http.Response, error) {
	upstreamURL := p.upstreamURL(r)

	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL.String(), body)
	if err != nil {
		return nil, fmt.Errorf("creating upstream request: %w", err)
	}

	copyRequestHeaders(r, upReq)
	p.injectAuth(upReq)

	return p.client.Do(upReq)
}

// injectAuth adds the appropriate authorization header.
func (p *Proxy) injectAuth(r *http.Request) {
	switch p.config.AuthType {
	case "bearer":
		r.Header.Set("Authorization", "Bearer "+p.config.Token)
	default: // "basic"
		creds := p.config.Username + ":" + p.config.Token
		encoded := base64.StdEncoding.EncodeToString([]byte(creds))
		r.Header.Set("Authorization", "Basic "+encoded)
	}
}

// copyRequestHeaders copies headers from the client request to the upstream request,
// skipping hop-by-hop headers.
func copyRequestHeaders(from *http.Request, to *http.Request) {
	for k, vals := range from.Header {
		switch strings.ToLower(k) {
		case "host", "authorization", "connection", "keep-alive",
			"proxy-authenticate", "proxy-authorization", "te", "trailer",
			"transfer-encoding", "upgrade":
			continue
		}
		for _, v := range vals {
			to.Header.Add(k, v)
		}
	}
	to.ContentLength = from.ContentLength
}

// relayResponse writes the upstream response back to the client.
func relayResponse(w http.ResponseWriter, resp *http.Response) {
	for k, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

