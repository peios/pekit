//! The `pekit` command (see [`pekit::cli`]).

use std::process::ExitCode;

fn main() -> ExitCode {
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
            pekit::cli::run(
                std::env::args_os(),
                &cwd,
                &mut std::io::stdout(),
                &mut std::io::stderr(),
            )
        });
    if let Ok(Ok(code)) = run.map(std::thread::JoinHandle::join) {
        code
    } else {
        eprintln!("error: pekit stopped unexpectedly");
        ExitCode::FAILURE
    }
}
