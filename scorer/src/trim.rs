//! Turning the site's free-text trim field into something comparable.
//! Port of `src/trim.js`.
//!
//! The vocabulary is induced from the corpus: tokens that recur across many
//! listings of the same model are trims, one-off sales copy is not. The noise
//! list only covers words that are frequent but still not trims.

use std::collections::{HashMap, HashSet};
use std::sync::OnceLock;

use regex::Regex;
use unicode_normalization::UnicodeNormalization;

use crate::js::{is_js_space, JS_SPACE_CLASS};

const NOISE: &[&str] = &[
    // drivetrain
    "awd", "fwd", "rwd", "4wd", "4x4", "4x2", "2wd", "4rm", "2rm", "integrale", "intégrale",
    "traction", "quattro", "xdrive", "4matic", "sh",
    // transmission
    "cvt", "auto", "automatic", "automatique", "manual", "manuel", "manuelle", "man",
    "at", "mt", "speed", "vitesses", "transmission",
    // body / cab / box
    "sedan", "sdn", "berline", "hatchback", "hayon", "coupe", "coupé", "wagon", "familiale",
    "supercrew", "supercab", "crewcab", "crew", "cab", "cabine", "quad", "king", "regular",
    "double", "extended", "allongee", "allongée", "box", "caisse", "bed", "pi", "ft", "door",
    "dr", "portes", "porte", "convertible", "cabriolet", "decapotable", "décapotable",
    "vus", "suv", "van", "fourgonnette", "camionnette", "pickup",
    // powertrain descriptors
    "hybrid", "hybride", "diesel", "essence", "gas", "turbo", "ecoboost", "tdi", "phev",
    "electric", "electrique", "électrique", "ev", "v6", "v8", "i4", "l4", "hemi", "cyl",
    "cylindres", "moteur", "engine",
    // generic filler
    "edition", "package", "pkg", "group", "groupe", "plus", "premium", "special",
    "new", "neuf", "used", "usage", "usagé", "km", "kms", "kilometrage", "kilométrage",
    "bas", "low", "certified", "certifie", "certifié", "garantie", "warranty",
    "inspecte", "inspecté", "financement", "credit", "crédit", "aubaine", "promo",
    "ensemble", "equipement", "equipe", "blackpack",
    // French grammar ("le" deliberately absent: it is a Toyota trim)
    "de", "du", "des", "la", "les", "aux", "avec", "sans", "pour", "et", "d", "l",
    // options and features
    "mags", "toit", "ouvrant", "cuir", "sieges", "sièges", "chauffants", "camera",
    "caméra", "recul", "navigation", "gps", "demarreur", "démarreur", "distance",
    "bluetooth", "air", "climatiseur", "climatisation", "ac", "ecran", "écran",
    "apple", "android", "carplay", "nav", "sunroof", "leather", "heated", "seats",
    "backup", "roof", "wheels", "night", "po", "pouces", "boite", "boîte",
    "pano", "panoramique", "panoramic", "chauffant", "chauffante", "conv", "cpe",
    "cabrio", "fastback", "vhr", "ta", "ti", "a",
    // condition and sales claims
    "carfax", "clean", "propre", "accidente", "accidenté", "accident", "jamais",
    "haut", "niveau", "litres", "litre", "years", "ans", "pieds", "pied", "impeccable",
];

struct Patterns {
    noise: HashSet<&'static str>,
    head: Regex,
    words: Regex,
    year: Regex,
    measure: Regex,
    door_count: Regex,
    package_code: Regex,
}

fn patterns() -> &'static Patterns {
    static P: OnceLock<Patterns> = OnceLock::new();
    P.get_or_init(|| Patterns {
        noise: NOISE.iter().copied().collect(),
        // Sales copy is usually appended after a dash or comma; keep the head.
        head: Regex::new(&format!(r"[{s}]+[-–—][{s}]+|,|/|\||\(", s = JS_SPACE_CLASS)).unwrap(),
        // Apostrophes join words that are separate: "d'équipement".
        words: Regex::new(&format!(r"[{}'’]+", JS_SPACE_CLASS)).unwrap(),
        year: Regex::new(r"^(19|20)[0-9]{2}$").unwrap(),
        measure: Regex::new(r#"(?i)^[0-9]+([.,][0-9]+)?('|"|l|t)?$"#).unwrap(),
        door_count: Regex::new(r"(?i)^[0-9]+(d|dr|p|portes?)$").unwrap(),
        package_code: Regex::new(r"(?i)^[0-9]{3}[a-z]$").unwrap(),
    })
}

/// `/^[A-HJ-NPR-Z0-9]{17}$/i` — a VIN.
fn is_vin(raw: &str) -> bool {
    let chars: Vec<char> = raw.chars().collect();
    chars.len() == 17
        && chars.iter().all(|c| {
            let u = c.to_ascii_uppercase();
            u.is_ascii_digit() || (u.is_ascii_uppercase() && !matches!(u, 'I' | 'O' | 'Q'))
        })
}

/// Lowercase, strip accents, drop everything except `a-z0-9-`.
pub fn normalize_token(token: &str) -> String {
    let stripped: String = token.nfd().filter(|c| !('\u{300}'..='\u{36f}').contains(c)).collect();
    stripped
        .to_lowercase()
        .chars()
        .filter(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || *c == '-')
        .collect()
}

#[derive(Debug, Clone, PartialEq)]
pub struct Token {
    pub raw: String,
    pub key: String,
}

/// Split raw trim text into candidate tokens, dropping what is clearly not a trim.
pub fn tokenize(text: Option<&str>, make: Option<&str>, model: Option<&str>) -> Vec<Token> {
    let text = match text {
        Some(t) if !t.trim_matches(is_js_space).is_empty() => t,
        _ => return vec![],
    };
    let p = patterns();
    let head = p.head.find(text).map_or(text, |m| &text[..m.start()]);
    let bare = |v: Option<&str>| match v {
        Some(s) if !s.is_empty() => Some(normalize_token(s).replace('-', "")),
        _ => None,
    };
    let model_key = bare(model);
    let make_key = bare(make);

    p.words
        .split(head)
        .map(|raw| Token { raw: raw.to_string(), key: normalize_token(raw) })
        .filter(|t| {
            let key = t.key.as_str();
            if key.is_empty() || key == "-" {
                return false;
            }
            if p.noise.contains(key)
                || p.year.is_match(key)
                || is_vin(&t.raw)
                || p.measure.is_match(key)
                || p.door_count.is_match(key)
                || p.package_code.is_match(key)
            {
                return false;
            }
            // The vehicle's own name is not a trim: "Civic Sedan", "F150".
            let flat = key.replace('-', "");
            if model_key.as_deref() == Some(flat.as_str()) || make_key.as_deref() == Some(flat.as_str()) {
                return false;
            }
            true
        })
        .collect()
}

/// Insertion-ordered counter, so ties sort exactly as a JS `Map` would.
#[derive(Default)]
struct Counter {
    order: Vec<String>,
    counts: HashMap<String, usize>,
}

impl Counter {
    fn add(&mut self, key: &str) {
        match self.counts.get_mut(key) {
            Some(n) => *n += 1,
            None => {
                self.order.push(key.to_string());
                self.counts.insert(key.to_string(), 1);
            }
        }
    }
    fn get(&self, key: &str) -> usize {
        self.counts.get(key).copied().unwrap_or(0)
    }
    fn kept(&self, threshold: usize) -> Vec<(String, usize)> {
        self.order
            .iter()
            .map(|k| (k.clone(), self.counts[k]))
            .filter(|(_, n)| *n >= threshold)
            .collect()
    }
}

#[derive(Debug, Clone, Copy, PartialEq)]
pub struct InduceOptions {
    pub min_count: f64,
    pub min_share: f64,
}

impl Default for InduceOptions {
    fn default() -> Self {
        InduceOptions { min_count: 3.0, min_share: 0.01 }
    }
}

/// Learn which token sequences are real trims for one model.
pub fn induce_vocabulary<'a, I>(
    trim_texts: I,
    make: Option<&str>,
    model: Option<&str>,
    opts: InduceOptions,
) -> Vec<String>
where
    I: IntoIterator<Item = Option<&'a str>>,
{
    let mut unigrams = Counter::default();
    let mut bigrams = Counter::default();
    let mut total = 0usize;

    for text in trim_texts {
        total += 1;
        let tokens = tokenize(text, make, model);
        let mut seen: Vec<String> = Vec::new();
        let mut push = |s: String| {
            if !seen.contains(&s) {
                seen.push(s);
            }
        };
        for (i, token) in tokens.iter().enumerate() {
            // Only the first few tokens can be a trim; past that it is description.
            if i > 2 {
                continue;
            }
            push(token.key.clone());
            if i + 1 < tokens.len() && i < 2 {
                push(format!("{} {}", token.key, tokens[i + 1].key));
            }
        }
        for candidate in seen {
            if candidate.contains(' ') {
                bigrams.add(&candidate);
            } else {
                unigrams.add(&candidate);
            }
        }
    }

    let threshold = opts.min_count.max((total as f64 * opts.min_share).ceil());
    let threshold = if threshold <= 0.0 { 0 } else { threshold.ceil() as usize };

    let uni = unigrams.kept(threshold);
    let bi: Vec<(String, usize)> = bigrams
        .kept(threshold)
        .into_iter()
        .filter(|(key, count)| {
            // Keep "Big Horn" only if it covers a real share of its first word's listings.
            let first = key.split(' ').next().unwrap_or("");
            let first_count = unigrams.get(first) as f64;
            *count as f64 >= opts.min_count.max(first_count * 0.25)
        })
        .collect();

    // Drop unigrams that only exist as halves of a kept bigram.
    let bigram_parts: HashSet<&str> = bi.iter().flat_map(|(k, _)| k.split(' ')).collect();
    let standalone: Vec<(String, usize)> = uni
        .into_iter()
        .filter(|(key, count)| {
            if !bigram_parts.contains(key.as_str()) {
                return true;
            }
            let partner = bi
                .iter()
                .find(|(b, _)| b.split(' ').any(|part| part == key))
                .map_or(0, |(_, n)| *n);
            *count as f64 > partner as f64 * 1.5
        })
        .collect();

    // Longest first so "sport touring" wins over "sport". Stable, like Array.sort.
    let mut all: Vec<(String, usize)> = bi.into_iter().chain(standalone).collect();
    all.sort_by(|a, b| {
        let wa = a.0.split(' ').count();
        let wb = b.0.split(' ').count();
        wb.cmp(&wa).then(b.1.cmp(&a.1))
    });
    all.into_iter().map(|(k, _)| k).collect()
}

/// Assign a canonical (upper-case) trim, or `None` when nothing matches.
pub fn assign_trim(
    trim_text: Option<&str>,
    vocabulary: &[String],
    make: Option<&str>,
    model: Option<&str>,
) -> Option<String> {
    let tokens = tokenize(trim_text, make, model);
    if tokens.is_empty() {
        return None;
    }
    let keys: Vec<&str> = tokens.iter().map(|t| t.key.as_str()).collect();
    for entry in vocabulary {
        let parts: Vec<&str> = entry.split(' ').collect();
        let mut i = 0;
        while i + parts.len() <= keys.len() {
            if parts.iter().enumerate().all(|(j, part)| keys[i + j] == *part) {
                return Some(entry.to_uppercase());
            }
            i += 1;
        }
    }
    None
}

#[cfg(test)]
mod tests {
    use super::*;

    fn keys(text: &str) -> Vec<String> {
        tokenize(Some(text), None, None).into_iter().map(|t| t.key).collect()
    }
    fn keys_for(text: &str, make: Option<&str>, model: Option<&str>) -> Vec<String> {
        tokenize(Some(text), make, model).into_iter().map(|t| t.key).collect()
    }
    fn vocab(texts: &[&str], model: Option<&str>) -> Vec<String> {
        induce_vocabulary(texts.iter().map(|t| Some(*t)), None, model, InduceOptions::default())
    }
    fn repeat(text: &'static str, n: usize) -> Vec<&'static str> {
        vec![text; n]
    }

    #[test]
    fn keeps_a_clean_trim() {
        assert_eq!(keys("XLE"), vec!["xle"]);
        assert_eq!(keys("EX-L"), vec!["ex-l"]);
    }

    #[test]
    fn normalizes_case_and_accents() {
        assert_eq!(keys("LARIAT"), keys("Lariat"));
        assert_eq!(normalize_token("Québec"), "quebec");
    }

    #[test]
    fn drops_drivetrain_body_and_transmission() {
        assert_eq!(keys("XLT 4WD SuperCrew 6.5' Box"), vec!["xlt"]);
        assert_eq!(keys("LX CVT"), vec!["lx"]);
        assert_eq!(keys("XLT cabine SuperCrew 4RM caisse de 6,5 pi"), vec!["xlt"]);
        assert!(keys("Ensemble d'équipement").is_empty());
    }

    #[test]
    fn drops_sales_pitch_vins_years_doors_and_packages() {
        assert_eq!(keys("LE AWD - Ultra low Kms - Remote Starter - Snows"), vec!["le"]);
        assert!(keys("4D, Man GS, Sun Roof, A/C").is_empty());
        assert!(keys("JM1BM1M3XE1210873").is_empty());
        assert_eq!(keys("Sport GS AWD 2019"), vec!["sport", "gs"]);
        assert_eq!(keys("4dr Sdn Auto GS"), vec!["gs"]);
        assert_eq!(keys("XLT 302A"), vec!["xlt"]);
    }

    #[test]
    fn drops_the_vehicles_own_name() {
        assert!(keys_for("Civic Sedan", None, Some("Civic")).is_empty());
        assert!(keys_for("F150", None, Some("F-150")).is_empty());
        assert_eq!(keys_for("Mazda3 GX", None, Some("Mazda3")), vec!["gx"]);
        assert_eq!(keys_for("Ram 1500 Rebel", Some("RAM"), Some("1500")), vec!["rebel"]);
    }

    #[test]
    fn keeps_le_and_handles_empty_input() {
        assert_eq!(keys("LE"), vec!["le"]);
        assert!(keys("").is_empty());
        assert!(keys("   ").is_empty());
        assert!(tokenize(None, None, None).is_empty());
    }

    #[test]
    fn induces_recurring_trims_only() {
        let mut texts = repeat("XLE", 10);
        texts.extend(repeat("LE", 5));
        texts.push("Ultra Rare Special Thing");
        let v = vocab(&texts, Some("RAV4"));
        assert!(v.contains(&"xle".to_string()));
        assert!(v.contains(&"le".to_string()));
        assert!(!v.iter().any(|t| t.contains("rare")));
    }

    #[test]
    fn learns_multi_word_trims_and_drops_their_halves() {
        let v = vocab(&repeat("Big Horn", 20), Some("1500"));
        assert!(v.contains(&"big horn".to_string()));
        assert!(!v.contains(&"big".to_string()));
        assert!(!v.contains(&"horn".to_string()));
    }

    #[test]
    fn orders_longer_trims_first() {
        let mut texts = repeat("Sport", 20);
        texts.extend(repeat("Sport Touring", 8));
        let v = vocab(&texts, Some("Civic"));
        let pos = |k: &str| v.iter().position(|x| x == k).unwrap();
        assert!(pos("sport touring") < pos("sport"));
    }

    #[test]
    fn requires_absolute_and_proportional_floors() {
        let mut texts = repeat("XLT", 1000);
        texts.extend(["Blorp", "Blorp"]);
        let v = vocab(&texts, Some("F-150"));
        assert!(v.contains(&"xlt".to_string()));
        assert!(!v.contains(&"blorp".to_string()));
        assert!(vocab(&["Alpha", "Beta", "Gamma"], None).is_empty());
    }

    #[test]
    fn assigns_trims() {
        let v: Vec<String> = ["sport touring", "ex-l", "lx", "ex", "touring", "sport", "si"]
            .iter()
            .map(|s| s.to_string())
            .collect();
        assert_eq!(assign_trim(Some("LX"), &v, None, None).as_deref(), Some("LX"));
        assert_eq!(assign_trim(Some("lx cvt"), &v, None, None).as_deref(), Some("LX"));
        assert_eq!(assign_trim(Some("Sport Touring"), &v, None, None).as_deref(), Some("SPORT TOURING"));
        assert_eq!(assign_trim(Some("4dr Sdn Auto EX"), &v, None, None).as_deref(), Some("EX"));
        assert_eq!(assign_trim(Some("Bas kilométrage"), &v, None, None), None);
        assert_eq!(assign_trim(Some(""), &v, None, None), None);
        assert_eq!(assign_trim(Some("LX"), &[], None, None), None);
    }

    #[test]
    fn messy_rav4_corpus_resolves() {
        let mut texts = repeat("XLE", 10);
        texts.extend(repeat("XSE", 6));
        texts.extend(repeat("LE", 5));
        texts.extend(repeat("Base", 4));
        texts.extend(repeat("SE", 3));
        texts.extend(repeat("Limited", 3));
        texts.extend(["AWD XLE", "XLE PREMIUM AWD", "XLE HYBRID", "SE AWD", "LE AWD - Ultra low Kms - Remote Starter - Snows"]);
        let v = induce_vocabulary(texts.iter().map(|t| Some(*t)), Some("Toyota"), Some("RAV4"), InduceOptions::default());
        let a = |t: &str| assign_trim(Some(t), &v, Some("Toyota"), Some("RAV4"));
        assert_eq!(a("AWD XLE").as_deref(), Some("XLE"));
        assert_eq!(a("XLE PREMIUM AWD").as_deref(), Some("XLE"));
        assert_eq!(a("SE AWD").as_deref(), Some("SE"));
        assert_eq!(a("LE AWD - Ultra low Kms - Remote Starter - Snows").as_deref(), Some("LE"));
    }
}
