//! What the tests that need something of this machine share: skipping,
//! when it isn't here, so that a skip can't pass for a test that ran.

use std::io::Write;

/// Says that the test calling it is skipped, for want of `what`, before
/// it returns. Cargo can't count a test skipped once it has started, and
/// reports a pass, so this writes to the standard error itself, which the
/// test harness shows; it keeps a passing test's `eprintln!` to itself.
/// With `$PEKIT_TEST_NO_SKIP` set, as `tools/check` sets it, the test
/// fails instead.
pub fn skip(what: &str) {
    let test = std::thread::current().name().unwrap_or("a test").to_owned();
    assert!(
        std::env::var_os("PEKIT_TEST_NO_SKIP").is_none(),
        "{test} can't run: {what}, and $PEKIT_TEST_NO_SKIP is set"
    );
    let _ = writeln!(
        std::io::stderr(),
        "SKIPPED, NOT PASSED: {test}: {what}; set $PEKIT_TEST_NO_SKIP to fail instead"
    );
}
