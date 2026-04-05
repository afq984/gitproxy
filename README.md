# gitproxy

A credential-holding reverse proxy for Git over HTTP that allows reads and prompts for human approval before publishing code to remote branches. Designed for AI agent sandboxes where credentials should not be exposed to the agent.

## Problem and threat model

AI agents running in sandboxes need to interact with Git repositories. Giving the agent direct access to credentials (tokens, SSH keys) is a security risk — the agent could exfiltrate them, use them for unintended purposes, or publish arbitrary code to remote branches without oversight.

The primary risk this proxy addresses is publication control: preventing an agent from making commits visible on remote branches, especially branches that may be used for pull requests or otherwise become visible to humans or automation.

This proxy is not a substitute for code review. Review of commit contents happens out of band. The write gate answers a narrower question:

- may a specific target commit be published to branch `refs/heads/X`?

For the initial implementation, read access is trusted and allowed automatically. Write approval is based on the target ref and destination object ID, not on an inline diff review.

## Solution

A lightweight reverse proxy that sits between the agent and the Git remote. It:

- Injects credentials into requests so the agent never sees them
- Allows read operations (clone, fetch, pull) to pass through automatically
- Intercepts write operations (push) and holds them for human approval before publication

The agent interacts with normal GitHub URLs. Git's `insteadOf` config transparently rewrites them to hit the proxy.

## Architecture

```
Agent sandbox                          Proxy (trusted)                    Remote
git push ──► http://localhost:8080/ ──► approve/deny? ──► https://github.com/
             (plain HTTP, no creds)     (injects auth)    (real credentials)
```

## Git smart HTTP protocol

Git over HTTP uses a small set of endpoints. The proxy classifies them as read or write based on the service name, not the HTTP method.

| Endpoint                                  | Method | Classification                    |
|-------------------------------------------|--------|-----------------------------------|
| `/repo/info/refs?service=git-upload-pack`  | GET    | Read (allow)                      |
| `/repo/git-upload-pack`                    | POST   | Read (allow)                      |
| `/repo/info/refs?service=git-receive-pack` | GET    | Write preflight (allow or log)    |
| `/repo/git-receive-pack`                   | POST   | Write (gate and enforce)          |

Dumb HTTP protocol requests (direct GET for objects/packs) are read-only by nature and always allowed.

## Agent-side configuration

The agent's Git config uses `insteadOf` to transparently redirect requests:

```gitconfig
[url "http://localhost:8080/"]
    insteadOf = https://github.com/
```

No changes to the agent's workflow, tooling, or URLs are required.

## Credential management

The proxy holds credentials and injects them into forwarded requests. Supported auth methods:

- **Personal access token** — injected as `Authorization: Basic` (username + token)
- **OAuth / fine-grained token** — injected as `Authorization: Bearer`

Credentials are configured on the proxy, never exposed to the agent. The proxy communicates with the remote over HTTPS.

## Approval flow for writes

The enforcement point is `POST /repo/git-receive-pack`. `GET /repo/info/refs?service=git-receive-pack` may be allowed through as capability discovery traffic, but it is not sufficient for authorization because it does not contain the actual ref update commands.

When a write operation is intercepted:

1. The proxy parses the `git-receive-pack` request payload to extract the proposed ref update command set.
2. For the initial implementation, the proxy supports exactly one ref update per push.
3. The proxy extracts and presents this approval tuple to a human reviewer:
   - target ref, for example `refs/heads/wip-1`
   - destination object ID (`new_oid`), for example the target commit to be published
   - source object ID (`old_oid`) for operator visibility only
4. The reviewer approves or denies publication of that tuple.
5. If approved, the proxy creates an in-memory single-use approval token bound to the exact approved tuple and forwards the request with credentials to the remote.
6. If denied, the proxy returns an HTTP error and git reports a push failure.
7. If no response is received within a configurable timeout (default: 5 minutes), the request is denied automatically.

### Approval semantics

The initial approval rule is intentionally narrow:

- allow publication only when `ref == approved_ref`
- allow publication only when `new_oid == approved_new_oid`
- do not require `old_oid` to match; the approval means "allow pushing this commit to this branch from any prior branch state"

This supports the intended workflow of approving publication of a specific commit to a specific branch even if the branch is created, updated, or force-pushed.

An approval token is:

- single-use
- held only in memory
- consumed only after confirmed upstream success
- invalid after timeout

If the upstream push is definitively rejected, the token is not consumed.

If the proxy loses connectivity or otherwise cannot determine whether the upstream accepted the push after the request was sent, the token enters an ambiguous state and must not be silently reused. The proxy should log the ambiguity clearly and require manual resolution or an explicit operator retry decision.

### Approval interface

The initial implementation should use a simple CLI prompt on the proxy's terminal. The proxy prints the proposed publication tuple and waits for `y/n` input.

Future iterations could support:

- A web dashboard with a queue of pending requests
- Webhook notifications (Slack, Discord)

## Functional requirements

1. Proxy listens on a configurable local port (default `8080`).
2. Forwards all requests to a configurable upstream (default `https://github.com`).
3. Injects authorization headers into all forwarded requests.
4. Allows all read operations without intervention.
5. Intercepts `POST .../git-receive-pack`, parses the proposed ref update, and requires human approval before forwarding.
6. Logs all requests (method, path, classification, outcome) to stdout.
7. Returns meaningful git errors to the agent on denied, timed-out, or ambiguous pushes.
8. Consumes an approval token only after confirmed upstream success.

## Non-functional requirements

- Single binary or single-file script with minimal dependencies.
- No persistent state — pending requests are held in memory.
- Handles one push at a time (single-agent use case).

## Out of scope (for now)

- Git over SSH
- Multiple remotes / credential mapping per destination
- Fine-grained path-based rules (e.g., allow push to some branches)
- Web-based approval UI
- Forward proxy mode for non-git traffic
- Diff preview in the approval prompt
