package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const gitHTTPBackend = "/usr/lib/git-core/git-http-backend"

func TestMain(m *testing.M) {
	if _, err := os.Stat(gitHTTPBackend); err != nil {
		fmt.Fprintf(os.Stderr, "skipping: git-http-backend not found at %s\n", gitHTTPBackend)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// testApprover is an Approver for tests that returns a preconfigured result.
type testApprover struct {
	approve      bool
	delay        time.Duration
	called       chan RefUpdate
	pairCodeChan chan string
}

func (a *testApprover) Approve(ctx context.Context, update RefUpdate, pairCode string) (bool, error) {
	if a.called != nil {
		a.called <- update
	}
	if a.pairCodeChan != nil {
		a.pairCodeChan <- pairCode
	}
	if a.delay > 0 {
		select {
		case <-time.After(a.delay):
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	return a.approve, nil
}

// testEnv holds the test infrastructure: upstream git-http-backend, proxy, and repo paths.
type testEnv struct {
	upstream   *httptest.Server
	proxy      *httptest.Server
	bareRepo   string
	proxyURL   string
	t          *testing.T
}

func newTestEnv(t *testing.T, approver Approver) *testEnv {
	t.Helper()

	bareRepo := t.TempDir()

	// Initialize a bare repo with at least one commit so clone works.
	run(t, bareRepo, "git", "init", "--bare", "--initial-branch=main", bareRepo)
	run(t, bareRepo, "git", "config", "--file", filepath.Join(bareRepo, "config"), "http.receivepack", "true")

	// Create an initial commit by cloning, committing, and pushing.
	workDir := t.TempDir()
	run(t, workDir, "git", "clone", bareRepo, workDir)
	run(t, workDir, "git", "config", "user.email", "test@test.com")
	run(t, workDir, "git", "config", "user.name", "Test")
	run(t, workDir, "git", "checkout", "-b", "main")
	writeFile(t, filepath.Join(workDir, "README"), "hello\n")
	run(t, workDir, "git", "add", "README")
	run(t, workDir, "git", "commit", "-m", "initial")
	run(t, workDir, "git", "push", "origin", "main")

	// Start git-http-backend as CGI behind an HTTP server.
	backend := &cgi.Handler{
		Path: gitHTTPBackend,
		Env: []string{
			"GIT_PROJECT_ROOT=" + filepath.Dir(bareRepo),
			"GIT_HTTP_EXPORT_ALL=1",
		},
		// git-http-backend expects PATH_INFO to include the repo name.
		// We strip it from the URL path.
	}

	// Wrap to set required CGI variables.
	upstreamHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// git-http-backend expects the repo to be identified via PATH_INFO.
		// Our setup uses the bare repo directory name as the repo name.
		repoName := filepath.Base(bareRepo)
		if !strings.HasPrefix(r.URL.Path, "/"+repoName) {
			http.NotFound(w, r)
			return
		}
		backend.Env = []string{
			"GIT_PROJECT_ROOT=" + filepath.Dir(bareRepo),
			"GIT_HTTP_EXPORT_ALL=1",
		}
		backend.ServeHTTP(w, r)
	})

	upstream := httptest.NewServer(upstreamHandler)
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)

	cfg := ProxyConfig{
		Upstream:        upstreamURL,
		AuthType:        "basic",
		Token:           "test-token",
		Username:        "test-user",
		ApprovalTimeout: 5 * time.Second,
	}

	proxy := NewProxy(cfg, approver)
	proxySrv := httptest.NewServer(proxy)
	t.Cleanup(proxySrv.Close)

	return &testEnv{
		upstream: upstream,
		proxy:    proxySrv,
		bareRepo: bareRepo,
		proxyURL: proxySrv.URL + "/" + filepath.Base(bareRepo),
		t:        t,
	}
}

func run(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, out)
	}
	return string(out)
}

func runGit(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestCloneThroughProxy(t *testing.T) {
	env := newTestEnv(t, &testApprover{approve: true})

	cloneDir := t.TempDir()
	run(t, cloneDir, "git", "clone", env.proxyURL, cloneDir)

	// Verify the cloned content.
	content, err := os.ReadFile(filepath.Join(cloneDir, "README"))
	if err != nil {
		t.Fatalf("reading cloned README: %v", err)
	}
	if string(content) != "hello\n" {
		t.Errorf("unexpected README content: %q", content)
	}
}

func TestFetchThroughProxy(t *testing.T) {
	env := newTestEnv(t, &testApprover{approve: true})

	// Clone first.
	cloneDir := t.TempDir()
	run(t, cloneDir, "git", "clone", env.proxyURL, cloneDir)

	// Add a commit directly to the bare repo via a separate work tree.
	tmpWork := t.TempDir()
	run(t, tmpWork, "git", "clone", env.bareRepo, tmpWork)
	run(t, tmpWork, "git", "config", "user.email", "test@test.com")
	run(t, tmpWork, "git", "config", "user.name", "Test")
	writeFile(t, filepath.Join(tmpWork, "file2"), "new content\n")
	run(t, tmpWork, "git", "add", "file2")
	run(t, tmpWork, "git", "commit", "-m", "add file2")
	run(t, tmpWork, "git", "push", "origin", "main")

	// Fetch through the proxy.
	run(t, cloneDir, "git", "fetch", "origin")

	// Verify the new commit is available.
	out := run(t, cloneDir, "git", "log", "--oneline", "origin/main")
	if !strings.Contains(out, "add file2") {
		t.Errorf("fetch did not get new commit, log: %s", out)
	}
}

func TestPushApproved(t *testing.T) {
	called := make(chan RefUpdate, 1)
	env := newTestEnv(t, &testApprover{approve: true, called: called})

	cloneDir := t.TempDir()
	run(t, cloneDir, "git", "clone", env.proxyURL, cloneDir)
	run(t, cloneDir, "git", "config", "user.email", "test@test.com")
	run(t, cloneDir, "git", "config", "user.name", "Test")
	writeFile(t, filepath.Join(cloneDir, "newfile"), "data\n")
	run(t, cloneDir, "git", "add", "newfile")
	run(t, cloneDir, "git", "commit", "-m", "add newfile")

	// Push through the proxy.
	run(t, cloneDir, "git", "push", "origin", "main")

	// Verify approval was requested.
	select {
	case update := <-called:
		if update.Ref != "refs/heads/main" {
			t.Errorf("unexpected ref: %s", update.Ref)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("approval was not requested")
	}

	// Verify the push landed in the bare repo.
	tmpWork := t.TempDir()
	run(t, tmpWork, "git", "clone", env.bareRepo, tmpWork)
	content, err := os.ReadFile(filepath.Join(tmpWork, "newfile"))
	if err != nil {
		t.Fatalf("newfile not found in bare repo: %v", err)
	}
	if string(content) != "data\n" {
		t.Errorf("unexpected newfile content: %q", content)
	}
}

func TestPushDenied(t *testing.T) {
	env := newTestEnv(t, &testApprover{approve: false})

	cloneDir := t.TempDir()
	run(t, cloneDir, "git", "clone", env.proxyURL, cloneDir)
	run(t, cloneDir, "git", "config", "user.email", "test@test.com")
	run(t, cloneDir, "git", "config", "user.name", "Test")
	writeFile(t, filepath.Join(cloneDir, "newfile"), "data\n")
	run(t, cloneDir, "git", "add", "newfile")
	run(t, cloneDir, "git", "commit", "-m", "add newfile")

	// Push should fail.
	out, err := runGit(t, cloneDir, "push", "origin", "main")
	if err == nil {
		t.Fatal("push should have failed but succeeded")
	}
	if !strings.Contains(out, "denied") {
		t.Errorf("expected denial message in output, got: %s", out)
	}

	// Verify the push did NOT land in the bare repo.
	tmpWork := t.TempDir()
	run(t, tmpWork, "git", "clone", env.bareRepo, tmpWork)
	if _, err := os.Stat(filepath.Join(tmpWork, "newfile")); err == nil {
		t.Error("newfile should not exist in bare repo after denied push")
	}
}

func TestPushTimeout(t *testing.T) {
	env := newTestEnv(t, &testApprover{approve: true, delay: 10 * time.Second})
	// Override approval timeout to be very short.
	env.proxy.Close()

	upstreamURL, _ := url.Parse(env.upstream.URL)
	cfg := ProxyConfig{
		Upstream:        upstreamURL,
		AuthType:        "basic",
		Token:           "test-token",
		Username:        "test-user",
		ApprovalTimeout: 100 * time.Millisecond,
	}
	proxy := NewProxy(cfg, &testApprover{approve: true, delay: 10 * time.Second})
	proxySrv := httptest.NewServer(proxy)
	t.Cleanup(proxySrv.Close)
	proxyURL := proxySrv.URL + "/" + filepath.Base(env.bareRepo)

	cloneDir := t.TempDir()
	run(t, cloneDir, "git", "clone", proxyURL, cloneDir)
	run(t, cloneDir, "git", "config", "user.email", "test@test.com")
	run(t, cloneDir, "git", "config", "user.name", "Test")
	writeFile(t, filepath.Join(cloneDir, "newfile"), "data\n")
	run(t, cloneDir, "git", "add", "newfile")
	run(t, cloneDir, "git", "commit", "-m", "add newfile")

	// Push should fail due to timeout.
	out, err := runGit(t, cloneDir, "push", "origin", "main")
	if err == nil {
		t.Fatal("push should have failed due to timeout but succeeded")
	}
	if !strings.Contains(out, "timed out") {
		t.Errorf("expected timeout message in output, got: %s", out)
	}
}

func TestPushNewBranch(t *testing.T) {
	called := make(chan RefUpdate, 1)
	env := newTestEnv(t, &testApprover{approve: true, called: called})

	cloneDir := t.TempDir()
	run(t, cloneDir, "git", "clone", env.proxyURL, cloneDir)
	run(t, cloneDir, "git", "config", "user.email", "test@test.com")
	run(t, cloneDir, "git", "config", "user.name", "Test")
	run(t, cloneDir, "git", "checkout", "-b", "feature-branch")
	writeFile(t, filepath.Join(cloneDir, "feature"), "feature content\n")
	run(t, cloneDir, "git", "add", "feature")
	run(t, cloneDir, "git", "commit", "-m", "add feature")

	// Push new branch through the proxy.
	run(t, cloneDir, "git", "push", "origin", "feature-branch")

	// Verify approval was requested with correct ref.
	select {
	case update := <-called:
		if update.Ref != "refs/heads/feature-branch" {
			t.Errorf("unexpected ref: %s", update.Ref)
		}
		// For new branch, old OID should be all zeros.
		if update.OldOID != strings.Repeat("0", 40) {
			t.Errorf("expected zero old OID for new branch, got: %s", update.OldOID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("approval was not requested")
	}
}

func TestPushPairingCode(t *testing.T) {
	pairCodeChan := make(chan string, 1)
	env := newTestEnv(t, &testApprover{approve: true, pairCodeChan: pairCodeChan})

	cloneDir := t.TempDir()
	run(t, cloneDir, "git", "clone", env.proxyURL, cloneDir)
	run(t, cloneDir, "git", "config", "user.email", "test@test.com")
	run(t, cloneDir, "git", "config", "user.name", "Test")
	writeFile(t, filepath.Join(cloneDir, "pairtest"), "data\n")
	run(t, cloneDir, "git", "add", "pairtest")
	run(t, cloneDir, "git", "commit", "-m", "test pairing code")

	// Capture stderr from git push to check for the pairing code.
	out, err := runGit(t, cloneDir, "push", "origin", "main")
	if err != nil {
		t.Fatalf("push failed: %v\n%s", err, out)
	}

	// The pairing code should appear as "remote: Pairing code: XXX-1234".
	if !strings.Contains(out, "Pairing code:") {
		t.Errorf("git push output should contain pairing code, got:\n%s", out)
	}

	// Verify the same code was passed to the approver.
	select {
	case code := <-pairCodeChan:
		if !strings.Contains(out, code) {
			t.Errorf("pairing code mismatch: approver got %q but git output was:\n%s", code, out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pairing code was not passed to approver")
	}
}

func TestUpstreamURLPathTraversal(t *testing.T) {
	upstream, _ := url.Parse("https://host/base")
	cfg := ProxyConfig{
		Upstream: upstream,
		AuthType: "basic",
		Token:    "t",
		Username: "u",
	}
	p := NewProxy(cfg, &testApprover{})

	tests := []struct {
		reqPath string
		want    string
	}{
		{"/repo/info/refs", "/base/repo/info/refs"},
		{"/../other/info/refs", "/base/other/info/refs"},
		{"/../../etc/passwd", "/base/etc/passwd"},
		{"/repo/../other/info/refs", "/base/other/info/refs"},
		{"/repo/info/refs", "/base/repo/info/refs"},
	}

	for _, tt := range tests {
		r, _ := http.NewRequest("GET", tt.reqPath, nil)
		got := p.upstreamURL(r)
		if got.Path != tt.want {
			t.Errorf("upstreamURL(%q).Path = %q, want %q", tt.reqPath, got.Path, tt.want)
		}
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		method string
		path   string
		query  string
		want   string
	}{
		{"GET", "/repo/info/refs", "service=git-upload-pack", "read"},
		{"POST", "/repo/git-upload-pack", "", "read"},
		{"GET", "/repo/info/refs", "service=git-receive-pack", "write-preflight"},
		{"POST", "/repo/git-receive-pack", "", "write"},
		{"GET", "/repo/objects/pack/pack-abc.pack", "", "read"},
		{"GET", "/repo/HEAD", "", "read"},
		{"GET", "/repo/objects/ab/cdef1234", "", "read"},
		{"GET", "/repo/info/packs", "", "read"},
		// Non-git endpoints should be unknown.
		{"GET", "/api/v3/repos/owner/repo", "", "unknown"},
		{"POST", "/api/v3/repos/owner/repo/issues", "", "unknown"},
		{"DELETE", "/api/v3/repos/owner/repo/branches/main", "", "unknown"},
		{"GET", "/repo/settings", "", "unknown"},
		// Wrong methods for git endpoints.
		{"GET", "/repo/git-upload-pack", "", "unknown"},
		{"GET", "/repo/git-receive-pack", "", "unknown"},
		{"POST", "/repo/info/refs", "service=git-receive-pack", "unknown"},
		{"DELETE", "/repo/info/refs", "", "unknown"},
	}

	for _, tt := range tests {
		r, _ := http.NewRequest(tt.method, tt.path+"?"+tt.query, nil)
		got := classify(r)
		if got != tt.want {
			t.Errorf("classify(%s %s?%s) = %q, want %q", tt.method, tt.path, tt.query, got, tt.want)
		}
	}
}

func TestParsePushRequest(t *testing.T) {
	// Build a realistic pkt-line payload.
	oldOID := strings.Repeat("a", 40)
	newOID := strings.Repeat("b", 40)
	ref := "refs/heads/main"
	line := fmt.Sprintf("%s %s %s\x00 report-status side-band-64k\n", oldOID, newOID, ref)
	pkt := fmt.Sprintf("%04x%s", len(line)+4, line)
	payload := pkt + "0000" + "packfile-data-here"

	push, err := parsePushRequest(strings.NewReader(payload))
	if err != nil {
		t.Fatalf("parsePushRequest: %v", err)
	}
	if len(push.updates) != 1 {
		t.Fatalf("expected 1 update, got %d", len(push.updates))
	}
	if push.updates[0].OldOID != oldOID || push.updates[0].NewOID != newOID || push.updates[0].Ref != ref {
		t.Errorf("unexpected update: %+v", push.updates[0])
	}
	if !push.hasCap("report-status") {
		t.Error("expected report-status capability")
	}
	if !push.hasCap("side-band-64k") {
		t.Error("expected side-band-64k capability")
	}

	// Verify that body() replays the full request.
	all, _ := io.ReadAll(push.body())
	if !strings.HasPrefix(string(all), pkt) {
		t.Error("body() should start with the command prefix")
	}
	if !strings.HasSuffix(string(all), "packfile-data-here") {
		t.Error("body() should end with the packfile data")
	}
}

func TestParsePushRequestMultipleUpdates(t *testing.T) {
	var buf bytes.Buffer
	for i := 0; i < 3; i++ {
		oldOID := strings.Repeat(fmt.Sprintf("%x", i), 40)[:40]
		newOID := strings.Repeat(fmt.Sprintf("%x", i+3), 40)[:40]
		ref := fmt.Sprintf("refs/heads/branch-%d", i)
		line := fmt.Sprintf("%s %s %s\n", oldOID, newOID, ref)
		if i == 0 {
			line = fmt.Sprintf("%s %s %s\x00 report-status\n", oldOID, newOID, ref)
		}
		fmt.Fprintf(&buf, "%04x%s", len(line)+4, line)
	}
	buf.WriteString("0000")

	push, err := parsePushRequest(&buf)
	if err != nil {
		t.Fatalf("parsePushRequest: %v", err)
	}
	if len(push.updates) != 3 {
		t.Fatalf("expected 3 updates, got %d", len(push.updates))
	}
}

func TestCredentialInjectionBasic(t *testing.T) {
	env := newTestEnv(t, &testApprover{approve: true})

	// Verify the proxy injects auth by checking the upstream received the header.
	var receivedAuth string
	origHandler := env.upstream.Config.Handler
	env.upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		origHandler.ServeHTTP(w, r)
	})

	cloneDir := t.TempDir()
	run(t, cloneDir, "git", "clone", env.proxyURL, cloneDir)

	if receivedAuth == "" {
		t.Fatal("upstream did not receive Authorization header")
	}
	if !strings.HasPrefix(receivedAuth, "Basic ") {
		t.Errorf("expected Basic auth, got: %s", receivedAuth)
	}
}

func TestMultipleRefUpdateRejected(t *testing.T) {
	// Build a payload with 2 ref updates.
	var buf bytes.Buffer
	for i := 0; i < 2; i++ {
		oldOID := strings.Repeat("0", 40)
		newOID := strings.Repeat(fmt.Sprintf("%x", i+1), 40)[:40]
		ref := fmt.Sprintf("refs/heads/branch-%d", i)
		line := fmt.Sprintf("%s %s %s\n", oldOID, newOID, ref)
		if i == 0 {
			line = fmt.Sprintf("%s %s %s\x00 report-status\n", oldOID, newOID, ref)
		}
		fmt.Fprintf(&buf, "%04x%s", len(line)+4, line)
	}
	buf.WriteString("0000")

	// Create a proxy and send the request directly.
	upstreamURL, _ := url.Parse("http://localhost:0")
	cfg := ProxyConfig{
		Upstream:        upstreamURL,
		AuthType:        "basic",
		Token:           "test-token",
		Username:        "test-user",
		ApprovalTimeout: 5 * time.Second,
	}
	proxy := NewProxy(cfg, &testApprover{approve: true})
	srv := httptest.NewServer(proxy)
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/repo/git-receive-pack", &buf)
	req.Header.Set("Content-Type", "application/x-git-receive-pack-request")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "single ref update") {
		t.Errorf("expected single ref update error, got: %s", body)
	}
}

func TestNonGitEndpointReturns403(t *testing.T) {
	env := newTestEnv(t, &testApprover{approve: true})

	paths := []struct {
		method string
		path   string
	}{
		{"GET", "/api/v3/repos/owner/repo"},
		{"POST", "/api/v3/repos/owner/repo/issues"},
		{"DELETE", "/api/v3/repos/owner/repo/branches/main"},
		{"GET", "/settings"},
		{"POST", "/repo/info/refs?service=git-receive-pack"},
	}

	for _, tt := range paths {
		req, _ := http.NewRequest(tt.method, env.proxy.URL+tt.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: request failed: %v", tt.method, tt.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s: got status %d, want 403", tt.method, tt.path, resp.StatusCode)
		}
	}

	// Also verify that the upstream never received any of these requests
	// (i.e., no credential leakage to non-git paths).
	var upstreamHit bool
	origHandler := env.upstream.Config.Handler
	env.upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit = true
		origHandler.ServeHTTP(w, r)
	})

	req, _ := http.NewRequest("GET", env.proxy.URL+"/api/v3/user", nil)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if upstreamHit {
		t.Error("non-git request was forwarded to upstream")
	}
}

func TestCredentialInjectionBearer(t *testing.T) {
	env := newTestEnv(t, &testApprover{approve: true})

	// Recreate proxy with bearer auth.
	env.proxy.Close()
	upstreamURL, _ := url.Parse(env.upstream.URL)
	cfg := ProxyConfig{
		Upstream:        upstreamURL,
		AuthType:        "bearer",
		Token:           "ghp_testtoken123",
		Username:        "ignored",
		ApprovalTimeout: 5 * time.Second,
	}
	proxy := NewProxy(cfg, &testApprover{approve: true})
	proxySrv := httptest.NewServer(proxy)
	t.Cleanup(proxySrv.Close)
	proxyURL := proxySrv.URL + "/" + filepath.Base(env.bareRepo)

	var receivedAuth string
	origHandler := env.upstream.Config.Handler
	env.upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		origHandler.ServeHTTP(w, r)
	})

	cloneDir := t.TempDir()
	run(t, cloneDir, "git", "clone", proxyURL, cloneDir)

	if receivedAuth == "" {
		t.Fatal("upstream did not receive Authorization header")
	}
	if receivedAuth != "Bearer ghp_testtoken123" {
		t.Errorf("expected 'Bearer ghp_testtoken123', got: %s", receivedAuth)
	}
}

func TestCLIApproverStaleInputAfterTimeout(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	defer pw.Close()

	approver := &CLIApprover{Reader: pr}
	update := RefUpdate{
		OldOID: strings.Repeat("0", 40),
		NewOID: strings.Repeat("a", 40),
		Ref:    "refs/heads/main",
	}

	// First approval: times out quickly.
	ctx1, cancel1 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel1()
	_, err = approver.Approve(ctx1, update, "TEST-0001")
	if err == nil {
		t.Fatal("first approval should have timed out")
	}

	// Simulate stale input arriving after timeout.
	// Write "y" — this should be consumed by the old (abandoned) channel.
	fmt.Fprintln(pw, "y")

	// Give the scanner goroutine time to consume the stale line.
	time.Sleep(50 * time.Millisecond)

	// Second approval: should get fresh input, not the stale "y".
	// Write "n" — this should go to the new channel.
	go func() {
		time.Sleep(20 * time.Millisecond)
		fmt.Fprintln(pw, "n")
	}()

	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	approved, err := approver.Approve(ctx2, update, "TEST-0002")
	if err != nil {
		t.Fatalf("second approval should not error: %v", err)
	}
	if approved {
		t.Error("second approval should be denied (got stale 'y' instead of fresh 'n')")
	}
}
