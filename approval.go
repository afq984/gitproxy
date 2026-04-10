package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// RefUpdate represents a proposed ref update from a git push.
type RefUpdate struct {
	OldOID string
	NewOID string
	Ref    string
}

func (r RefUpdate) String() string {
	return fmt.Sprintf("%s %s %s", r.OldOID, r.NewOID, r.Ref)
}

// pushRequest holds the parsed command prefix of a git-receive-pack request.
type pushRequest struct {
	updates      []RefUpdate
	capabilities []string
	// cmdPrefix contains the buffered pkt-line commands (including flush).
	// rest is the remaining unbuffered body (packfile data).
	cmdPrefix []byte
	rest      io.Reader
}

// body returns a reader that replays the full request body:
// the buffered command prefix followed by the remaining data.
func (pr *pushRequest) body() io.Reader {
	return io.MultiReader(bytes.NewReader(pr.cmdPrefix), pr.rest)
}

// Approver decides whether a push should be allowed.
// The pairCode is a short identifier displayed to both the git client
// and the reviewer so they can verify the prompt matches the push.
type Approver interface {
	Approve(ctx context.Context, update RefUpdate, pairCode string) (bool, error)
}

// CLIApprover prompts on stdin/stdout for approval.
// Each Approve call creates a fresh channel; the shared scanner goroutine
// atomically switches to the new channel, so stale input from a timed-out
// prompt is discarded rather than consumed by a later prompt.
type CLIApprover struct {
	once     sync.Once
	mu       sync.Mutex    // serializes Approve calls
	activeCh atomic.Value  // holds chan string for the current prompt
	Reader   io.Reader     // input source; defaults to os.Stdin if nil
}

func (a *CLIApprover) init() {
	a.once.Do(func() {
		r := a.Reader
		if r == nil {
			r = os.Stdin
		}
		go func() {
			scanner := bufio.NewScanner(r)
			for scanner.Scan() {
				ch, ok := a.activeCh.Load().(chan string)
				if !ok {
					continue
				}
				select {
				case ch <- scanner.Text():
				default:
					// Channel full (stale prompt), discard input.
				}
			}
		}()
	})
}

func (a *CLIApprover) Approve(ctx context.Context, update RefUpdate, pairCode string) (bool, error) {
	a.init()
	a.mu.Lock()
	defer a.mu.Unlock()

	// Create a fresh buffered channel for this prompt.
	// The buffer allows the scanner goroutine to deliver one line
	// without blocking, even if this Approve has already returned.
	ch := make(chan string, 1)
	a.activeCh.Store(ch)

	fmt.Println()
	fmt.Println("=== Push approval required ===")
	fmt.Printf("  Pairing code: %s\n", pairCode)
	fmt.Printf("  Ref:          %s\n", update.Ref)
	fmt.Printf("  New OID:      %s\n", update.NewOID)
	fmt.Printf("  Old OID:      %s\n", update.OldOID)
	fmt.Print("Approve? [y/N] ")

	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case line := <-ch:
		return strings.TrimSpace(strings.ToLower(line)) == "y", nil
	}
}

// parsePushRequest reads the pkt-line command prefix from a git-receive-pack
// request body. Only the small command section is buffered; the packfile
// data remains unread in the returned pushRequest.rest reader.
func parsePushRequest(body io.Reader) (*pushRequest, error) {
	br := bufio.NewReader(body)
	req := &pushRequest{}
	var cmdBuf bytes.Buffer
	first := true

	for {
		// Read the 4-byte pkt-line length header.
		var hdr [4]byte
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			return nil, fmt.Errorf("reading pkt-line header: %w", err)
		}
		cmdBuf.Write(hdr[:])

		hexLen := string(hdr[:])
		if hexLen == "0000" {
			break // flush packet — end of commands
		}

		pktLen, err := strconv.ParseInt(hexLen, 16, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid pkt-line length %q: %w", hexLen, err)
		}
		if pktLen < 4 {
			return nil, fmt.Errorf("pkt-line length %d too small", pktLen)
		}

		dataLen := int(pktLen) - 4
		payload := make([]byte, dataLen)
		if _, err := io.ReadFull(br, payload); err != nil {
			return nil, fmt.Errorf("reading pkt-line data: %w", err)
		}
		cmdBuf.Write(payload)

		// Strip trailing newline for parsing.
		trimmed := bytes.TrimRight(payload, "\n")

		// First pkt-line contains capabilities after null byte.
		refPayload := trimmed
		if first {
			if idx := bytes.IndexByte(trimmed, 0); idx >= 0 {
				refPayload = trimmed[:idx]
				caps := string(trimmed[idx+1:])
				req.capabilities = strings.Fields(strings.TrimSpace(caps))
			}
			first = false
		} else if idx := bytes.IndexByte(trimmed, 0); idx >= 0 {
			refPayload = trimmed[:idx]
		}

		fields := strings.Fields(string(refPayload))
		if len(fields) != 3 {
			return nil, fmt.Errorf("expected 3 fields in ref update, got %d: %q", len(fields), string(refPayload))
		}

		req.updates = append(req.updates, RefUpdate{
			OldOID: fields[0],
			NewOID: fields[1],
			Ref:    fields[2],
		})
	}

	req.cmdPrefix = cmdBuf.Bytes()
	req.rest = br
	return req, nil
}

// hasCap returns true if the push request includes the given capability.
func (pr *pushRequest) hasCap(cap string) bool {
	for _, c := range pr.capabilities {
		if c == cap {
			return true
		}
	}
	return false
}

// writeReportStatus writes a git report-status error response.
// If sideband is true, the response is wrapped in sideband channel 1.
func writeReportStatus(w http.ResponseWriter, ref, msg string, sideband bool) {
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.WriteHeader(http.StatusOK)

	// Build report-status pkt-lines.
	var rs bytes.Buffer
	writePktLineStr(&rs, fmt.Sprintf("unpack %s\n", msg))
	if ref != "" {
		writePktLineStr(&rs, fmt.Sprintf("ng %s %s\n", ref, msg))
	}
	rs.WriteString("0000")

	if sideband {
		// Wrap in sideband channel 1.
		data := rs.Bytes()
		sbPkt := make([]byte, 0, len(data)+1)
		sbPkt = append(sbPkt, 1) // band 1 = pack data / report-status
		sbPkt = append(sbPkt, data...)
		fmt.Fprintf(w, "%04x", len(sbPkt)+4)
		w.Write(sbPkt)
		w.Write([]byte("0000"))
	} else {
		w.Write(rs.Bytes())
	}
}

func writePktLineStr(buf *bytes.Buffer, s string) {
	fmt.Fprintf(buf, "%04x%s", len(s)+4, s)
}

// spoolPushBody writes the full push body (command prefix + packfile) to a
// temporary file, draining the client connection so it doesn't stall while
// the proxy waits for human approval. The caller must close and remove the file.
func spoolPushBody(push *pushRequest) (*os.File, error) {
	f, err := os.CreateTemp("", "gitproxy-push-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp file: %w", err)
	}
	// Write the command prefix.
	if _, err := f.Write(push.cmdPrefix); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, fmt.Errorf("writing command prefix: %w", err)
	}
	// Drain the remaining body (packfile data).
	if _, err := io.Copy(f, push.rest); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, fmt.Errorf("spooling packfile: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, fmt.Errorf("seeking temp file: %w", err)
	}
	return f, nil
}

// writeSidebandProgress writes a sideband channel-2 (progress) pkt-line.
// These appear as "remote: <msg>" in the git client's stderr.
func writeSidebandProgress(w io.Writer, msg string) {
	data := append([]byte{2}, []byte(msg)...)
	fmt.Fprintf(w, "%04x", len(data)+4)
	w.Write(data)
}

// writeSidebandReportStatus writes a report-status error response wrapped in
// sideband channel 1, followed by a flush packet. Unlike writeReportStatus,
// this writes only the body (no HTTP headers) — for use when the response has
// already been started.
func writeSidebandReportStatus(w io.Writer, ref, msg string) {
	var rs bytes.Buffer
	writePktLineStr(&rs, fmt.Sprintf("unpack %s\n", msg))
	if ref != "" {
		writePktLineStr(&rs, fmt.Sprintf("ng %s %s\n", ref, msg))
	}
	rs.WriteString("0000")

	data := append([]byte{1}, rs.Bytes()...)
	fmt.Fprintf(w, "%04x", len(data)+4)
	w.Write(data)
	w.Write([]byte("0000")) // flush
}

// handleWrite intercepts a git-receive-pack POST, parses the ref updates,
// requests approval, and either forwards or denies the push.
func (p *Proxy) handleWrite(w http.ResponseWriter, r *http.Request) {
	push, err := parsePushRequest(r.Body)
	if err != nil {
		log.Printf("error parsing ref updates: %v", err)
		http.Error(w, "proxy: malformed push payload", http.StatusBadRequest)
		return
	}

	sideband := push.hasCap("side-band-64k") || push.hasCap("side-band")

	if len(push.updates) == 0 {
		log.Println("push with no ref updates, forwarding")
		resp, err := p.forwardBody(r, push.body())
		if err != nil {
			log.Printf("upstream error: %v", err)
			writeReportStatus(w, "", "upstream connection failed", sideband)
			return
		}
		defer resp.Body.Close()
		relayResponse(w, resp)
		return
	}

	if len(push.updates) > 1 {
		log.Printf("push with %d ref updates, rejecting (only single ref update supported)", len(push.updates))
		// Drain the remaining body so the client doesn't get a broken pipe.
		io.Copy(io.Discard, push.rest)
		writeReportStatus(w, push.updates[0].Ref, "proxy: only single ref update per push is supported", sideband)
		return
	}

	update := push.updates[0]
	log.Printf("push: %s -> %s %s", update.OldOID, update.NewOID, update.Ref)

	// Spool the full body to a temp file so the client upload completes
	// before we block waiting for human approval.
	spool, err := spoolPushBody(push)
	if err != nil {
		log.Printf("error spooling push body: %v", err)
		writeReportStatus(w, update.Ref, "proxy: internal error", sideband)
		return
	}
	defer os.Remove(spool.Name())
	defer spool.Close()

	// Generate a pairing code so the reviewer can verify they are
	// approving the same push the git client is waiting on.
	pairCode := generatePairCode()
	log.Printf("pairing code for %s: %s", update.Ref, pairCode)

	// If the client supports sideband, start the HTTP response early
	// and send the pairing code as a progress message. The client
	// displays these as "remote: ..." lines.
	responseStarted := false
	if sideband {
		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		w.WriteHeader(http.StatusOK)
		responseStarted = true
		writeSidebandProgress(w, fmt.Sprintf("Pairing code: %s\n", pairCode))
		writeSidebandProgress(w, "Waiting for push approval...\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}

	// sendError writes a report-status error to the client. When the
	// response has already been started (sideband pairing code sent),
	// it writes only the sideband body; otherwise it writes a full
	// HTTP response.
	sendError := func(ref, msg string) {
		if responseStarted {
			writeSidebandReportStatus(w, ref, msg)
		} else {
			writeReportStatus(w, ref, msg, false)
		}
	}

	// Request approval with timeout.
	ctx, cancel := context.WithTimeout(r.Context(), p.config.ApprovalTimeout)
	defer cancel()

	approved, err := p.approver.Approve(ctx, update, pairCode)
	if err != nil {
		if ctx.Err() != nil {
			log.Printf("approval timed out for %s [%s]", update.Ref, pairCode)
			sendError(update.Ref, "proxy: approval timed out")
			return
		}
		log.Printf("approval error: %v", err)
		sendError(update.Ref, "proxy: approval error")
		return
	}

	if !approved {
		log.Printf("push denied for %s [%s]", update.Ref, pairCode)
		sendError(update.Ref, "proxy: push denied by reviewer")
		return
	}

	log.Printf("push approved for %s -> %s [%s]", update.NewOID, update.Ref, pairCode)

	// Forward the spooled body to upstream.
	resp, err := p.forwardBody(r, spool)
	if err != nil {
		log.Printf("upstream error after approval: %v", err)
		sendError(update.Ref, "proxy: upstream connection failed after approval (ambiguous state - verify manually)")
		return
	}
	defer resp.Body.Close()

	// Read the upstream response to log the outcome.
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("error reading upstream response: %v", err)
		sendError(update.Ref, "proxy: failed to read upstream response (ambiguous state - verify manually)")
		return
	}

	// If the upstream returned an HTTP-level error (e.g. 401, 500),
	// the body is likely not valid Git protocol. Send a proper
	// report-status error so the client gets a meaningful message.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("upstream HTTP error (status %d) for %s [%s]; approval token not consumed", resp.StatusCode, update.Ref, pairCode)
		sendError(update.Ref, fmt.Sprintf("proxy: upstream rejected push (HTTP %d)", resp.StatusCode))
		return
	}

	// Relay the response to the client. If we already started the
	// response (sideband pairing code), only write the body.
	if !responseStarted {
		for k, vals := range resp.Header {
			for _, v := range vals {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
	}
	w.Write(respBody)

	// Log outcome based on response.
	if strings.Contains(string(respBody), "unpack ok") {
		log.Printf("push succeeded upstream for %s -> %s [%s]", update.NewOID, update.Ref, pairCode)
	} else {
		log.Printf("push rejected upstream for %s [%s]; approval token not consumed", update.Ref, pairCode)
	}
}

// approvalToken represents a single-use approval bound to a specific ref update.
type approvalToken struct {
	update    RefUpdate
	createdAt time.Time
	consumed  bool
	ambiguous bool
}
