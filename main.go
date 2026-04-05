package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
)

func main() {
	var (
		listenAddr = flag.String("listen", ":8080", "address to listen on")
		upstream   = flag.String("upstream", "https://github.com", "upstream Git server URL")
		authType   = flag.String("auth-type", "basic", "authentication type: basic or bearer")
		token      = flag.String("token", "", "authentication token (PAT or OAuth token)")
		username   = flag.String("username", "", "username for basic auth (default: x-access-token)")
		timeout    = flag.Duration("approval-timeout", 5*60*1e9, "timeout for push approval")
	)
	flag.Parse()

	if *token == "" {
		fmt.Fprintln(os.Stderr, "error: -token is required")
		flag.Usage()
		os.Exit(1)
	}

	upstreamURL, err := url.Parse(*upstream)
	if err != nil {
		log.Fatalf("invalid upstream URL: %v", err)
	}

	if *username == "" {
		*username = "x-access-token"
	}

	cfg := ProxyConfig{
		Upstream:        upstreamURL,
		AuthType:        *authType,
		Token:           *token,
		Username:        *username,
		ApprovalTimeout: *timeout,
	}

	approver := &CLIApprover{}
	proxy := NewProxy(cfg, approver)

	log.Printf("gitproxy listening on %s, upstream %s", *listenAddr, *upstream)
	if err := http.ListenAndServe(*listenAddr, proxy); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
