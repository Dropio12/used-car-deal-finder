//! Small helpers that reproduce JavaScript number semantics.
//!
//! The scorer must give the same answers as the original JS code, down to the
//! last rounded dollar. These helpers exist so every place that did
//! `Math.round`, `String(n)` or `n.toLocaleString('fr-CA')` in JS does the
//! exact same thing here.

use serde_json::{Number, Value};

/// JavaScript `Math.round`: halves round toward +infinity (`-2.5` -> `-2`).
pub fn round(x: f64) -> f64 {
    if !x.is_finite() {
        return x;
    }
    let f = x.floor();
    if x - f >= 0.5 {
        f + 1.0
    } else {
        f
    }
}

/// JavaScript `String(n)` for the numbers this crate handles.
pub fn num_str(x: f64) -> String {
    if x.is_nan() {
        return "NaN".into();
    }
    if x.is_infinite() {
        return if x > 0.0 { "Infinity".into() } else { "-Infinity".into() };
    }
    if x == x.trunc() && x.abs() < 1e21 {
        return format!("{:.0}", x);
    }
    format!("{}", x)
}

/// JavaScript template-literal rendering of an optional string: `null` for missing.
pub fn opt_str(value: Option<&str>) -> String {
    value.map_or_else(|| "null".to_string(), str::to_string)
}

/// `n.toLocaleString('fr-CA')`: no-break-space groups, comma decimals, max 3 decimals.
pub fn fr_ca(x: f64) -> String {
    if !x.is_finite() {
        return num_str(x);
    }
    let negative = x < 0.0;
    let fixed = format!("{:.3}", x.abs());
    let (int_part, frac_part) = fixed.split_once('.').unwrap_or((fixed.as_str(), ""));
    let frac = frac_part.trim_end_matches('0');

    let digits: Vec<char> = int_part.chars().collect();
    let mut grouped = String::new();
    for (i, c) in digits.iter().enumerate() {
        if i > 0 && (digits.len() - i).is_multiple_of(3) {
            grouped.push('\u{a0}');
        }
        grouped.push(*c);
    }

    let mut out = String::new();
    if negative && (grouped != "0" || !frac.is_empty()) {
        out.push('-');
    }
    out.push_str(&grouped);
    if !frac.is_empty() {
        out.push(',');
        out.push_str(frac);
    }
    out
}

/// A JSON number that prints like JavaScript: integers without a trailing `.0`.
pub fn json_num(x: f64) -> Value {
    if x.is_finite() && x == x.trunc() && x.abs() < 9_007_199_254_740_992.0 {
        return Value::from(x as i64);
    }
    Number::from_f64(x).map_or(Value::Null, Value::Number)
}

pub fn json_opt_num(x: Option<f64>) -> Value {
    x.map_or(Value::Null, json_num)
}

/// JavaScript truthiness for a JSON value.
pub fn truthy(value: &Value) -> bool {
    match value {
        Value::Null => false,
        Value::Bool(b) => *b,
        Value::Number(n) => n.as_f64().is_some_and(|f| f != 0.0 && !f.is_nan()),
        Value::String(s) => !s.is_empty(),
        Value::Array(_) | Value::Object(_) => true,
    }
}

/// The characters JavaScript's `\s` and `String.prototype.trim` treat as whitespace.
pub fn is_js_space(c: char) -> bool {
    matches!(
        c,
        '\t' | '\n' | '\u{0B}' | '\u{0C}' | '\r' | ' ' | '\u{A0}' | '\u{1680}'
            | '\u{2000}'..='\u{200A}'
            | '\u{2028}' | '\u{2029}' | '\u{202F}' | '\u{205F}' | '\u{3000}' | '\u{FEFF}'
    )
}

/// JavaScript `\s` as a regex character-class body.
pub const JS_SPACE_CLASS: &str =
    r"\t\n\x0B\x0C\r \x{A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}\x{FEFF}";

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn round_matches_math_round() {
        assert_eq!(round(2.5), 3.0);
        assert_eq!(round(-2.5), -2.0);
        assert_eq!(round(0.499_999_999_999_999_94), 0.0);
        assert_eq!(round(-0.3), 0.0);
        assert_eq!(round(1234.49), 1234.0);
    }

    #[test]
    fn num_str_matches_string_of_number() {
        assert_eq!(num_str(2019.0), "2019");
        assert_eq!(num_str(-1234.0), "-1234");
        assert_eq!(num_str(0.5), "0.5");
    }

    #[test]
    fn fr_ca_groups_with_no_break_space() {
        assert_eq!(fr_ca(1234.0), "1\u{a0}234");
        assert_eq!(fr_ca(1_234_567.0), "1\u{a0}234\u{a0}567");
        assert_eq!(fr_ca(12.0), "12");
        assert_eq!(fr_ca(1234.5), "1\u{a0}234,5");
        assert_eq!(fr_ca(-1234.0), "-1\u{a0}234");
    }

    #[test]
    fn json_num_drops_trailing_zero() {
        assert_eq!(json_num(29480.0).to_string(), "29480");
        assert_eq!(json_num(0.25).to_string(), "0.25");
    }

    #[test]
    fn truthiness() {
        assert!(!truthy(&Value::Null));
        assert!(!truthy(&Value::from(0)));
        assert!(!truthy(&Value::from("")));
        assert!(truthy(&Value::from(true)));
        assert!(truthy(&Value::from(1)));
    }
}
