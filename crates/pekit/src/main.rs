//! The `pekit` command (see [`pekit::cli`]).

use std::io::IsTerminal;
use std::process::ExitCode;

fn main() -> ExitCode {
    // The meter a build runs each program of a step under, on this thread,
    // which the program dies with (see [`pekit::meter`]).
    let mut args = std::env::args_os();
    if args.nth(1).is_some_and(|word| word == pekit::meter::METER) {
        return pekit::meter::meter(args);
    }
    let cwd = match std::env::current_dir() {
        Ok(cwd) => cwd,
        Err(error) => {
            eprintln!("error: cannot tell the current directory: {error}");
            return ExitCode::FAILURE;
        }
    };
    let run = std::thread::Builder::new()
        .stack_size(gcsl::STACK_SIZE)
        .spawn(move || {
            let live = std::io::stderr().is_terminal();
            let mut terminal = pekit::cli::Terminal {
                out: &mut std::io::stdout(),
                err: &mut std::io::stderr(),
                live,
            };
            pekit::cli::run(std::env::args_os(), &cwd, &mut terminal)
        });
    if let Ok(Ok(code)) = run.map(std::thread::JoinHandle::join) {
        code
    } else {
        eprintln!("error: pekit stopped unexpectedly");
        ExitCode::FAILURE
    }
}
