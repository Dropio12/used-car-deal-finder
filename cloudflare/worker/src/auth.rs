//! Who may call what: bearer token for ingest, and where reads are served.

/// Host the Pages Function uses when it calls this Worker over the service
/// binding. A request can only carry this host when it comes through a
/// binding: the Cloudflare edge routes public traffic by real hostnames.
pub const INTERNAL_HOST: &str = "carbuyer-api.internal";

/// Read routes (/api/deals, /api/snapshots/latest) answer on the internal
/// host always, and on the public workers.dev host only when the
/// `PUBLIC_READ_API` var is "true" (local dev, or behind Cloudflare Access).
pub fn read_allowed(host: Option<&str>, public_read_api: bool) -> bool {
    public_read_api || host == Some(INTERNAL_HOST)
}

/// True when `header` is exactly `Bearer <expected>`. The comparison takes the
/// same time wherever the first difference is, so the token cannot be guessed
/// byte by byte from response times. An empty expected token never matches.
pub fn bearer_ok(header: Option<&str>, expected: &str) -> bool {
    if expected.is_empty() {
        return false;
    }
    let Some(given) = header.and_then(|h| h.strip_prefix("Bearer ")) else {
        return false;
    };
    constant_time_eq(given.trim().as_bytes(), expected.as_bytes())
}

fn constant_time_eq(a: &[u8], b: &[u8]) -> bool {
    // Length is not secret enough to hide; the content is.
    if a.len() != b.len() {
        return false;
    }
    a.iter().zip(b).fold(0u8, |acc, (x, y)| acc | (x ^ y)) == 0
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn accepts_only_the_exact_token() {
        assert!(bearer_ok(Some("Bearer s3cret"), "s3cret"));
        assert!(!bearer_ok(Some("Bearer s3creT"), "s3cret"));
        assert!(!bearer_ok(Some("Bearer s3cret2"), "s3cret"));
        assert!(!bearer_ok(Some("s3cret"), "s3cret"));
        assert!(!bearer_ok(Some("Basic s3cret"), "s3cret"));
        assert!(!bearer_ok(None, "s3cret"));
        assert!(!bearer_ok(Some("Bearer "), ""), "no configured token means ingest is closed");
    }

    #[test]
    fn reads_only_through_the_binding_unless_public() {
        assert!(read_allowed(Some(INTERNAL_HOST), false));
        assert!(!read_allowed(Some("carbuyer-api.me.workers.dev"), false));
        assert!(!read_allowed(None, false));
        assert!(read_allowed(Some("carbuyer-api.me.workers.dev"), true));
    }
}
