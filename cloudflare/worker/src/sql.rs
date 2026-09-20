//! Database-neutral statements: SQL text plus positional parameters.
//!
//! The planning code (ingest, snapshots) produces these without touching D1,
//! so it can be unit-tested natively. Only `d1.rs` turns them into D1 calls.

use serde_json::Value;

/// One bound value. D1 has no BigInt, so integers travel as JS numbers; every
/// value we store fits in 53 bits.
#[derive(Debug, Clone, PartialEq)]
pub enum Param {
    Null,
    Int(i64),
    Real(f64),
    Text(String),
}

impl Param {
    pub fn text(s: Option<&str>) -> Param {
        s.map_or(Param::Null, |s| Param::Text(s.to_string()))
    }

    /// A number bound the way SQLite stores it from JS (and from the Go store):
    /// whole numbers as INTEGER, anything else as REAL.
    pub fn num(f: Option<f64>) -> Param {
        match f {
            None => Param::Null,
            Some(f) if f.fract() == 0.0 && f.abs() < 9_007_199_254_740_992.0 => Param::Int(f as i64),
            Some(f) => Param::Real(f),
        }
    }

    pub fn as_f64(&self) -> Option<f64> {
        match self {
            Param::Int(i) => Some(*i as f64),
            Param::Real(f) => Some(*f),
            _ => None,
        }
    }

    pub fn as_str(&self) -> Option<&str> {
        match self {
            Param::Text(s) => Some(s),
            _ => None,
        }
    }
}

/// A statement ready to run: `?1`, `?2`, ... refer to `params` in order.
#[derive(Debug, Clone, PartialEq)]
pub struct Stmt {
    pub sql: String,
    pub params: Vec<Param>,
}

impl Stmt {
    pub fn new(sql: impl Into<String>, params: Vec<Param>) -> Stmt {
        Stmt { sql: sql.into(), params }
    }
}

/// `?1, ?2, ... ?n` starting at `from`.
pub fn placeholders(from: usize, n: usize) -> String {
    (from..from + n).map(|i| format!("?{i}")).collect::<Vec<_>>().join(", ")
}

/// A JSON number without a spurious `.0`: D1 hands every number back as a JS
/// double, so 2019 arrives as 2019.0.
pub fn json_num(f: f64) -> Value {
    if f.fract() == 0.0 && f.abs() < 9_007_199_254_740_992.0 {
        Value::from(f as i64)
    } else {
        serde_json::Number::from_f64(f).map_or(Value::Null, Value::Number)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn numbers_bind_like_sqlite_from_js() {
        assert_eq!(Param::num(Some(24000.0)), Param::Int(24000));
        assert_eq!(Param::num(Some(1.5)), Param::Real(1.5));
        assert_eq!(Param::num(None), Param::Null);
    }

    #[test]
    fn placeholders_are_positional() {
        assert_eq!(placeholders(1, 3), "?1, ?2, ?3");
        assert_eq!(placeholders(4, 1), "?4");
    }

    #[test]
    fn json_num_drops_trailing_zero() {
        assert_eq!(json_num(2019.0).to_string(), "2019");
        assert_eq!(json_num(0.25).to_string(), "0.25");
    }
}
