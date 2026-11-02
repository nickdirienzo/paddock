# GitHub App acceptance

Verified on 2026-11-02 using the real `paddock-eng` App, native Go authentication,
and the existing PR #1 watch. This supersedes the authentication limitations in
the [original coordination acceptance](github-acceptance.md).

## Configuration

- App ID: `5584529`; verified bot login: `paddock-eng[bot]`.
- Installation: `177939576`, on `nickdirienzo`.
- Named local profile: `personal`; route: `nickdirienzo/* → personal`.
- The previous `gh` profile is retained as `default`, with its wildcard route removed.
- App key imported outside the repository/task registry into protected user config
  storage: credential directory `0700`, key `0600`. No key or token is included here.

## Live checks

1. `github installations` signed a JWT and discovered the installation. `github auth`
   verified the App ID, derived its bot login, checked installation ownership, and
   successfully minted an installation access token without `gh` authentication.
2. `paddock github poll --json` authenticated through the mapped App and read the existing
   watch for https://github.com/nickdirienzo/paddock/pull/1. It returned `[]`: this PR was
   already merged, so no new agent action was appropriate. The opt-in live test also
   read both conversation comments and inline review comments through the App transport.
3. `paddock github reply` posted in the existing review thread as `paddock-eng[bot]`:
   https://github.com/nickdirienzo/paddock/pull/1#discussion_r4446745780.
   Independent GitHub inspection confirmed the bot author and reply parent
   `4445688929`. The existing event/task ownership was preserved.
4. The live test moved only the cached token deadline into the past. The next native
   request minted a different real token and successfully read the PR. It then revoked
   that test token with GitHub, confirmed HTTP 401, and verified automatic reminting and
   successful retry. This tests expiry handling without waiting an hour; the 401 was
   a real server rejection. Unit tests separately advance the signing/cache clock.
5. The live test supplied the actual PEM in the parent environment, then launched a
   probe executable using production `AgentCommand`. Its captured environment contained
   neither PEM nor key-variable references. No minted token or key appeared in the local
   GitHub journal. The probe tests the coding-agent process boundary without asking a
   model to handle a credential. Fixture tests additionally check generated arguments,
   prompts, task serialization, Git, terminal, and agent-control subprocess environments.

Run the explicit live check with an existing mapped App profile:

```sh
PADDOCK_LIVE_GITHUB_APP_PR=https://github.com/OWNER/REPO/pull/NUMBER \
  mise exec -- go test ./internal/paddock -run '^TestLiveGitHubApp$' -count=1 -v
```

It mints temporary tokens and revokes only a token it just minted. It does not post
comments. Normal test runs skip it. Output contains pass/fail evidence, never credentials.

## Automated validation

- Full Go test suite and race detector; `go vet ./...`.
- Linux amd64 and Windows amd64 CLI cross-compilation.
- JWT RSA signature/claims, supported PEM formats, wrong bot identity rejection,
  in-memory cache reuse, concurrent mint serialization, expiry refresh, one 401 retry,
  no mutation replay after 502, redirection refusal, and secret-free errors.
- Protected key import; loose permissions and symlinks rejected.
- Exact repository > owner wildcard > explicit default precedence; unmapped repos fail.
- Queued replies retain their original principal after route changes; replacing that
  principal blocks delivery. Legacy single-identity state preserves pending outbox identity.
- A real CLI subprocess with `PADDOCK_TASK_ID` queues an App reply without any key.
  Subsequent polling sends it once; repeat polling does not duplicate it.
- Existing authorization, planner, ownership, review threading, durable action claims,
  reconciliation, native-message adapter, and delegation tests continue to pass.

## Scope

No second Mirage App was needed for this live check. Personal/team routing and separate
principals are covered by automated tests; a future Mirage installation can be configured
with its own profile and `mirage-security/*` route.

Private keys stay out of inherited agent credentials and generated task data. Host
execution still shares same-user filesystem permissions; this is not an OS sandbox.
Agent App replies are queued for a credentialed poller. The implementation uses protected
files rather than Keychain; Windows currently uses environment PEM credentials.
