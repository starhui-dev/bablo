# ADR 0005: Web Session Authentication

- Status: Accepted
- Date: 2026-08-29

## Context

Bablo has two separate authentication surfaces: browser control-plane users and inference clients. Reusing inference API Keys as browser credentials would couple UI sessions to model entitlement, weaken revocation/CSRF semantics, and violate the boundary that one Key can access multiple models through policy. The first production stage may run one instance, but identity facts and revocation must remain correct when Redis is absent or lost.

Email delivery and an external identity provider are not yet available. Administrator access still needs a safe bootstrap/reset path and MFA enforcement without inventing a non-functional email recovery flow.

## Decision

1. Web management uses PostgreSQL-backed opaque Sessions; inference `/v1/*` continues to use separate Bablo API Keys.
2. Passwords use Argon2id PHC hashes with an explicit parameter version. The current profile is 19 MiB memory, two iterations, one lane, a 16-byte random salt, and a 32-byte derived key. Successful login rehashes older profiles.
3. Session and CSRF tokens are independent 32-byte CSPRNG values. PostgreSQL stores only SHA-256 hashes. Login and successful MFA rotate Sessions; logout, password change/reset, and logout-all revoke rows rather than deleting history.
4. Browser mutations require all of: a valid Session Cookie, exact allowed Origin, a CSRF Cookie matching `X-CSRF-Token`, and the CSRF hash bound to that server-side Session. Production Cookies are Secure; Session is HttpOnly and SameSite=Lax.
5. RBAC decisions live in `internal/auth.Service`, not the UI or handler. Production cannot disable admin MFA. Admin actions require the `admin` role plus an MFA-enabled, MFA-verified Session.
6. TOTP enrollment persists an AES-256-GCM encrypted pending secret, requires a valid TOTP confirmation, advances a row-locked counter to reject replay, creates ten hashed 80-bit recovery codes, and rotates the Session. Recovery codes are consumed with a conditional database update.
7. PostgreSQL is the identity, revocation, MFA, and audit fact source. P0 login/MFA throttling is a bounded, expiring process-local limiter suitable only for the declared single-instance stage. HA requires Redis coordination, but Redis never restores or grants identity state.
8. Until email/IdP infrastructure exists, trusted local operations use `bablo auth create-admin` and `bablo auth reset-password`. Password input is read from a no-echo terminal or stdin, never argv. Reset revokes all target Sessions and writes audit.
9. TOTP key material is injected as a 32-byte base64 environment secret with a key version. The schema records key version. Multi-key reads and background re-encryption must be implemented before rotating a production key.

## Consequences

- Browser compromise cannot read the HttpOnly Session token; CSRF requires both browser token possession and the Session-bound server hash.
- Database disclosure exposes password/Session/recovery hashes and encrypted TOTP material, not reusable plaintext credentials.
- Administrator accounts can sign in before enrollment but cannot execute admin operations until TOTP is confirmed.
- Password reset remains operational without pretending email delivery exists, but requires trusted host/database access and is unsuitable as end-user self-service.
- Process-local throttling is intentionally simple and observable for P0; multi-instance deployment is blocked until coordinated throttling is implemented.
- Losing the only active MFA encryption key makes TOTP and recovery verification unavailable. Production key rotation therefore requires multi-version decryption and a tested re-encryption runbook.

## Rejected alternatives

- JWT-only browser sessions: revocation and password-reset invalidation become delayed or require another fact store.
- Redis-only sessions: Redis loss would become an authorization event and violate PostgreSQL source-of-truth.
- API Keys for browser login: conflates control-plane identity with inference entitlement and creates unsafe token exposure in browser flows.
- Email reset placeholders: would create a misleading recovery promise without a verified delivery provider.
- Permanent account lockout: enables denial of service; bounded expiring throttles are used instead.
