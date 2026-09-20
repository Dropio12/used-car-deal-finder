//! Medians and nearest-rank percentiles. Medians, never means: one mistyped
//! price drags a mean badly and barely moves a median.

fn sorted(values: &[f64]) -> Vec<f64> {
    let mut v = values.to_vec();
    v.sort_by(|a, b| a.partial_cmp(b).unwrap_or(std::cmp::Ordering::Equal));
    v
}

pub fn median(values: &[f64]) -> Option<f64> {
    if values.is_empty() {
        return None;
    }
    let s = sorted(values);
    let mid = s.len() / 2;
    Some(if s.len() % 2 == 1 { s[mid] } else { (s[mid - 1] + s[mid]) / 2.0 })
}

/// Nearest-rank percentile: no interpolation, so it always returns a real observation.
pub fn percentile(values: &[f64], p: f64) -> Option<f64> {
    if values.is_empty() {
        return None;
    }
    let s = sorted(values);
    let rank = ((p / 100.0) * s.len() as f64).ceil() - 1.0;
    let rank = rank.max(0.0).min((s.len() - 1) as f64) as usize;
    Some(s[rank])
}

pub fn min(values: &[f64]) -> f64 {
    values.iter().copied().fold(f64::INFINITY, f64::min)
}

pub fn max(values: &[f64]) -> f64 {
    values.iter().copied().fold(f64::NEG_INFINITY, f64::max)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn median_of_odd_and_even_counts() {
        assert_eq!(median(&[3.0, 1.0, 2.0]), Some(2.0));
        assert_eq!(median(&[1.0, 2.0, 3.0, 4.0]), Some(2.5));
    }

    #[test]
    fn median_ignores_an_outlier() {
        assert_eq!(median(&[29_000.0, 30_000.0, 31_000.0, 30_500.0, 7_998.0]), Some(30_000.0));
    }

    #[test]
    fn percentile_returns_real_observations() {
        let v = [10.0, 20.0, 30.0, 40.0];
        assert_eq!(percentile(&v, 25.0), Some(10.0));
        assert_eq!(percentile(&v, 75.0), Some(30.0));
        assert_eq!(percentile(&v, 100.0), Some(40.0));
    }

    #[test]
    fn empty_input_is_none() {
        assert_eq!(median(&[]), None);
        assert_eq!(percentile(&[], 50.0), None);
    }
}
