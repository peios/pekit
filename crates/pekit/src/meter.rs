//! Metering: the program that runs each program of a step, measuring what
//! it used and lending it the budget's jobs.
//!
//! # Metering
//!
//! Where a build has a budget ([The budget](crate::budget#the-budget)), each
//! program of a step runs under pekit itself, `pekit __meter`, outside its
//! sandbox ([`Metered::wrap`]). It runs the program's bubblewrap, waits for
//! it, and keeps its peak resident size as the program's latest
//! ([Estimates](crate::budget#estimates)). It is a *subreaper*, so that the
//! process bubblewrap starts as the sandbox's `init`, which bubblewrap's
//! own process does not wait for, is its to wait for, and the peaks of the
//! program's processes reach it. It dies with the thread that started it,
//! and the program with it, as the program did without it.
//!
//! It exits as the program did: with its status, or, for one a signal
//! killed, 128 and the signal's number, which `exec` reports alike.
//!
//! # Jobservers
//!
//! It also offers the program a GNU make jobserver, a FIFO in the budget's
//! directory seen at [`JOBSERVER`], through `MAKEFLAGS` and
//! `CARGO_MAKEFLAGS`, `--jobserver-auth=fifo:/dev/pekit-jobserver`, unless
//! the program sets either itself ([`offer_jobserver`]). GNU make from 4.4,
//! Cargo, `rustc` and `cc` read it. A program holds one job of the budget
//! as it runs, which the protocol leaves implicit; each token in the FIFO
//! is a job more, which the meter took from the budget and lends. It lends
//! one while a job is free, no program waits for one, and the last it lent
//! was taken: so a program that reads none holds one job more for at most
//! [`SPARE_IDLE`] at a time, less often each time. Tokens given back are
//! lent again, or, once a program waits for the budget, given back to it.
//! Neither the jobserver nor these variables reach a receipt or a record.

use std::ffi::OsString;
use std::fs::{self, File};
use std::io::{self, Read as _, Write as _};
use std::os::unix::fs::OpenOptionsExt as _;
use std::path::{Path, PathBuf};
use std::process::{Command, ExitCode};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::thread;
use std::time::{Duration, Instant};

use nix::errno::Errno;
use nix::fcntl::OFlag;
use nix::sys::prctl;
use nix::sys::resource::{UsageWho, getrusage};
use nix::sys::signal::Signal;
use nix::sys::stat::Mode;
use nix::sys::wait::{WaitPidFlag, WaitStatus, waitpid};
use nix::unistd::mkfifo;

use crate::budget::{Budget, Limits, UNIT, keep_peak, program_name};

/// Where a step's program sees its jobserver's FIFO.
pub const JOBSERVER: &str = "/dev/pekit-jobserver";

/// The word `pekit` runs the meter by.
pub const METER: &str = "__meter";

/// How long a lent token may wait unread before it is given back.
pub const SPARE_IDLE: Duration = Duration::from_millis(250);

/// How long the meter waits, after a token went unread, before it lends
/// one again, doubled each time one does, and the longest it waits.
const SPARE_BACKOFF: Duration = Duration::from_millis(250);

const SPARE_BACKOFF_MOST: Duration = Duration::from_secs(4);

/// How often the meter looks at its FIFO.
const TICK: Duration = Duration::from_millis(10);

/// How long the meter waits for what the program left running once it has
/// ended: its sandbox's `init`, which dies with it.
const ORPHANS: Duration = Duration::from_secs(5);

/// The number of the next FIFO this process makes.
static FIFOS: AtomicU64 = AtomicU64::new(0);

/// How a step's programs are metered: the budget, and the step's name.
#[derive(Clone, Debug)]
pub struct Metered {
    /// The budget.
    pub budget: Arc<Budget>,
    /// The step's name, `<function>/<identity>`, its peaks are kept by.
    pub step: String,
}

impl Metered {
    /// Where a new program's jobserver's FIFO is to be, if its programs are
    /// metered: in the budget's directory, by this process's number.
    #[must_use]
    pub fn fifo(&self) -> Option<PathBuf> {
        self.budget.meter()?;
        let number = FIFOS.fetch_add(1, Ordering::Relaxed);
        Some(
            self.budget
                .dir()
                .join(format!("fifo-{}-{number}", std::process::id())),
        )
    }

    /// The command that runs `wrapped`, a sandbox's bubblewrap for the
    /// program `program`, under the meter, its jobserver at `fifo` (see
    /// [Metering](#metering)).
    #[must_use]
    pub fn wrap(&self, wrapped: &Command, program: &str, fifo: &Path) -> Command {
        let budget = &self.budget;
        let mut meter = Command::new(budget.meter().unwrap_or_else(|| Path::new("pekit")));
        meter
            .arg(METER)
            .arg(budget.dir())
            .arg(budget.limits().jobs.to_string())
            .arg(fifo)
            .arg(budget.peaks())
            .arg(&self.step)
            .arg(program_name(program))
            .arg(std::process::id().to_string())
            .arg("--")
            .arg(wrapped.get_program())
            .args(wrapped.get_args());
        meter.env_clear();
        for (name, value) in wrapped.get_envs() {
            if let Some(value) = value {
                meter.env(name, value);
            }
        }
        if let Some(dir) = wrapped.get_current_dir() {
            meter.current_dir(dir);
        }
        meter
    }
}

/// Sets `MAKEFLAGS` and `CARGO_MAKEFLAGS` in `wrapped`, a sandbox's
/// bubblewrap, to name the jobserver at `fifo`, bound at [`JOBSERVER`],
/// each unless `env`, the program's environment, sets it (see
/// [Jobservers](#jobservers)): never `-j`, which a make older than 4.4,
/// that reads no FIFO, would take as no limit at all.
pub fn offer_jobserver(wrapped: &mut Command, fifo: &Path, env: &[(String, Option<String>)]) {
    wrapped.arg("--bind").arg(fifo).arg(JOBSERVER);
    let flags = format!("--jobserver-auth=fifo:{JOBSERVER}");
    for name in ["MAKEFLAGS", "CARGO_MAKEFLAGS"] {
        if !env.iter().any(|(set, value)| set == name && value.is_some()) {
            wrapped.args(["--setenv", name, &flags]);
        }
    }
}

/// Removes each FIFO in the budget's directory `dir` whose process has
/// ended.
pub fn remove_stale_fifos(dir: &Path) {
    let Ok(entries) = fs::read_dir(dir) else {
        return;
    };
    for entry in entries.flatten() {
        let name = entry.file_name();
        let Some(pid) = name
            .to_str()
            .and_then(|name| name.strip_prefix("fifo-"))
            .and_then(|rest| rest.split('-').next())
        else {
            continue;
        };
        if !Path::new("/proc").join(pid).exists() {
            let _ = fs::remove_file(entry.path());
        }
    }
}

/// What the meter is told, after [`METER`].
struct Told {
    /// The budget's directory.
    budget: PathBuf,
    /// How many jobs it has.
    jobs: usize,
    /// Where its jobserver's FIFO is to be.
    fifo: PathBuf,
    /// Where peaks are kept.
    peaks: PathBuf,
    /// The step's name.
    step: String,
    /// The program's file name.
    program: String,
    /// The process that runs it, which it dies with.
    parent: u32,
    /// The command it runs.
    command: Vec<OsString>,
}

/// What `args`, the meter's, after [`METER`], tell it, if they are as
/// [`Metered::wrap`] writes them.
fn told(mut args: impl Iterator<Item = OsString>) -> Option<Told> {
    let mut next = || args.next();
    let budget = PathBuf::from(next()?);
    let jobs = next()?.to_str()?.parse().ok()?;
    let fifo = PathBuf::from(next()?);
    let peaks = PathBuf::from(next()?);
    let step = next()?.into_string().ok()?;
    let program = next()?.into_string().ok()?;
    let parent = next()?.to_str()?.parse().ok()?;
    if next()? != "--" {
        return None;
    }
    let command: Vec<OsString> = std::iter::from_fn(next).collect();
    (!command.is_empty()).then_some(Told {
        budget,
        jobs,
        fifo,
        peaks,
        step,
        program,
        parent,
        command,
    })
}

/// Runs the meter, `args` its arguments after [`METER`] (see
/// [Metering](#metering)): its exit is the program's.
#[must_use]
pub fn meter(args: impl Iterator<Item = OsString>) -> ExitCode {
    let Some(told) = told(args) else {
        eprintln!("pekit: `{METER}` is pekit's own, run by a build");
        return ExitCode::from(2);
    };
    // Dying with the thread that started it, as the program did without
    // it, unless that ended first.
    if prctl::set_pdeathsig(Signal::SIGKILL).is_err()
        || std::os::unix::process::parent_id() != told.parent
    {
        return ExitCode::from(128 + 9);
    }
    let _ = prctl::set_child_subreaper(true);
    let lender = Lender::start(&told);
    let mut command = Command::new(&told.command[0]);
    command.args(&told.command[1..]);
    let status = match command.status() {
        Ok(status) => status,
        Err(error) => {
            eprintln!(
                "pekit: cannot run `{}`: {error}",
                told.command[0].to_string_lossy()
            );
            return ExitCode::from(127);
        }
    };
    wait_for_orphans();
    if let Some(lender) = lender {
        lender.stop();
    }
    let _ = fs::remove_file(&told.fifo);
    if let Ok(usage) = getrusage(UsageWho::RUSAGE_CHILDREN) {
        let peak = u64::try_from(usage.max_rss()).unwrap_or_default() * 1024;
        let _ = keep_peak(&told.peaks, (&told.step, &told.program), peak);
    }
    let code = match (status.code(), std::os::unix::process::ExitStatusExt::signal(&status)) {
        (Some(code), _) => code,
        (None, Some(signal)) => 128 + signal,
        (None, None) => 1,
    };
    ExitCode::from(u8::try_from(code).unwrap_or(1))
}

/// Waits for each process the program left that is now this one's, its
/// sandbox's `init`, for [`ORPHANS`] at most: so that their peaks are
/// counted.
fn wait_for_orphans() {
    let until = Instant::now() + ORPHANS;
    while Instant::now() < until {
        match waitpid(None, Some(WaitPidFlag::WNOHANG)) {
            Ok(WaitStatus::StillAlive) => thread::sleep(Duration::from_millis(1)),
            Ok(_) | Err(Errno::EINTR) => {}
            Err(_) => return,
        }
    }
}

/// The thread that lends a program's jobserver the budget's jobs (see
/// [Jobservers](#jobservers)).
struct Lender {
    /// Set to stop it.
    stop: Arc<AtomicBool>,
    /// It.
    thread: thread::JoinHandle<()>,
}

impl Lender {
    /// The lender of `told`'s jobserver, its FIFO made: none if it cannot
    /// be, when the program has no tokens but its own.
    fn start(told: &Told) -> Option<Self> {
        let limits = Limits {
            jobs: told.jobs,
            memory: UNIT,
            unseen: UNIT,
        };
        let budget = Budget::open(&told.budget, limits, (&told.peaks, None)).ok()?;
        let _ = fs::remove_file(&told.fifo);
        mkfifo(&told.fifo, Mode::S_IRUSR | Mode::S_IWUSR).ok()?;
        // Read and written here, so that it is never at an end for the
        // program, and never waited on.
        let fifo = File::options()
            .read(true)
            .write(true)
            .custom_flags(OFlag::O_NONBLOCK.bits())
            .open(&told.fifo)
            .ok()?;
        let stop = Arc::new(AtomicBool::new(false));
        let stopping = Arc::clone(&stop);
        let thread = thread::spawn(move || lend(&budget, fifo, &stopping));
        Some(Self { stop, thread })
    }

    /// Stops it, every job it lent given back.
    fn stop(self) {
        self.stop.store(true, Ordering::Release);
        let _ = self.thread.join();
    }
}

/// Lends `fifo`, a jobserver, `budget`'s jobs, until `stop` is set (see
/// [Jobservers](#jobservers)), then gives back every job it lent.
fn lend(budget: &Budget, mut fifo: File, stop: &AtomicBool) {
    let most = budget.limits().jobs.saturating_sub(1);
    let mut lent: Vec<File> = Vec::new();
    // Since when a token has sat unread, if one has.
    let mut unread: Option<Instant> = None;
    let (mut backoff, mut next) = (SPARE_BACKOFF, Instant::now());
    while !stop.load(Ordering::Acquire) {
        thread::sleep(TICK);
        let now = Instant::now();
        let others = budget.others_wait().unwrap_or(false);
        let back = drained(&mut fifo);
        if back > 0 {
            // A token kept unread so long, or wanted elsewhere, goes back.
            let idle = unread.is_some_and(|since| now - since >= SPARE_IDLE);
            if idle {
                next = now + backoff;
                backoff = (backoff * 2).min(SPARE_BACKOFF_MOST);
            }
            let keep = usize::from(!others && !idle).min(lent.len());
            if keep == 1 && fifo.write_all(b"+").is_ok() {
                unread.get_or_insert(now);
            } else {
                unread = None;
            }
            let give = back.min(lent.len()) - keep.min(back);
            lent.truncate(lent.len() - give);
            continue;
        }
        // Every token lent is the program's: the last was taken.
        if unread.take().is_some() {
            backoff = SPARE_BACKOFF;
        }
        if lent.len() < most
            && now >= next
            && !others
            && let Ok(Some(job)) = budget.try_job()
            && fifo.write_all(b"+").is_ok()
        {
            lent.push(job);
            unread = Some(now);
        }
    }
    drained(&mut fifo);
}

/// How many tokens `fifo` held, each read.
fn drained(fifo: &mut File) -> usize {
    let mut tokens = [0u8; 64];
    let mut read = 0;
    loop {
        match fifo.read(&mut tokens) {
            Ok(0) => return read,
            Ok(count) => read += count,
            Err(error) if error.kind() == io::ErrorKind::Interrupted => {}
            Err(_) => return read,
        }
    }
}
