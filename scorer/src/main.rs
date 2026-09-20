//! `carbuyer-scorer`: reads listings as JSON on stdin, writes scores as JSON on stdout.
//!
//! Input: a JSON array of listings, or `{"listings": [...], "options": {...}}`
//! where `options` may hold `model` (price-model overrides such as
//! `{"minComps": 3}`), `discount` and `privateSources`.
//!
//! Output: `{"appraiser": {...summary...}, "scores": [{"id": ..., "score": {...} | null}]}`.

use std::io::{self, Read, Write};
use std::process::ExitCode;

fn main() -> ExitCode {
    if std::env::args().any(|a| a == "--help" || a == "-h") {
        println!("usage: carbuyer-scorer < listings.json > scores.json");
        return ExitCode::SUCCESS;
    }
    let mut raw = String::new();
    if let Err(e) = io::stdin().read_to_string(&mut raw) {
        eprintln!("carbuyer-scorer: cannot read stdin: {e}");
        return ExitCode::from(2);
    }
    let input: serde_json::Value = match serde_json::from_str(&raw) {
        Ok(v) => v,
        Err(e) => {
            eprintln!("carbuyer-scorer: stdin is not valid JSON: {e}");
            return ExitCode::from(2);
        }
    };
    match scorer::score::run(&input) {
        Ok(out) => {
            let mut stdout = io::stdout().lock();
            let written = serde_json::to_writer(&mut stdout, &out).map_err(io::Error::from);
            if written.and_then(|()| stdout.write_all(b"\n")).is_err() {
                return ExitCode::from(1);
            }
            ExitCode::SUCCESS
        }
        Err(e) => {
            eprintln!("carbuyer-scorer: {e}");
            ExitCode::from(2)
        }
    }
}
