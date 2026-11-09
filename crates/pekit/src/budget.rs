//! The machine's budget: how many programs, and how much memory, every
//! pekit on a machine may use at once.
//!
//! # The budget
//!
//! A build's pool runs at most `--jobs` effects at once, which bounds one
//! process; the budget bounds them all, each worktree's builds and each
//! agent's, so that twelve builds at once neither run twelve times as many
//! compilers as the machine has processors nor run it out of memory. It has
//! two limits ([`Limits`]): how many programs may run at once, `jobs`, and
//! how many bytes they may be expected to use, `memory`. Each program of a
//! step takes one of the first and as much of the second as it is expected
//! to use ([`Budget::estimate`]) before it runs, and gives them back when
//! it ends ([`Budget::admit`]). One that cannot have them waits, in the
//! order its process asked, holding none of them.
//!
//! Nothing runs to keep it: the budget is a directory of lock files
//! ([`budget_dir`]), `job-<n>` for each program that may run and
//! `memory-<n>` for each [`UNIT`] of memory, each held with `flock` by
//! whatever uses it. A process that ends, however it ends, lets go of what
//! it held. A program takes all it needs at once or none of it, so no two
//! programs wait for each other, and one expected to use more memory than
//! the budget has takes all of it, and so runs alone. One that has waited
//! for [`STARVED`] holds the directory's `gate`, so that no program
//! starts until it has started, which smaller programs starting one after
//! another would otherwise keep it from forever. Every process that waits
//! holds `waiting` shared, by which a program's jobserver knows to give
//! back what it borrowed ([Jobservers](crate::meter#jobservers)).
//!
//! Every process reads its limits for itself ([`Limits::of_machine`]), and
//! uses the slots numbered below them: two that read different limits
//! share the larger.
//!
//! None of this reaches a step's receipt or a program's record: the budget
//! changes when a program runs, never what it gives.
//!
//! # Estimates
//!
//! What a program is expected to use is what it used before. Each program
//! of a step is metered as it runs ([Metering](crate::meter#metering)), and
//! its peak resident size kept in the state directory, under `peaks/`, by
//! the step's name and the program's file name: for the step itself, and
//! for its *kind*, its function and that program, across every step of it.
//! A program is expected to use a quarter more than the most it used in the
//! last [`KEPT_PEAKS`] runs of it in that step; one never metered in that
//! step, a quarter more than the most its kind used; and one of a kind never
//! metered, [`Limits::unseen`].
//!
//! The peak is the largest single process's of the program's tree, as the
//! kernel counts it for the processes a parent waits for, not the tree's
//! sum: a `make -j` step's, whose jobs run beside each other, is low by as
//! many jobs as it runs at once.

use std::fs::{self, File, TryLockError};
use std::io;
use std::path::{Path, PathBuf};
use std::sync::{Condvar, Mutex, MutexGuard, PoisonError};
use std::time::{Duration, Instant};

use crate::evict::size_of;
use crate::store::Digest;

/// The memory one `memory-<n>` lock file stands for: 256 MiB.
pub const UNIT: u64 = 256 << 20;

/// What a program of a kind never metered is expected to use when nothing
/// sets [`Limits::unseen`]: 1 GiB.
pub const DEFAULT_UNSEEN: u64 = 1 << 30;

/// How long a program waits for the budget before it holds the gate (see
/// [The budget](#the-budget)).
pub const STARVED: Duration = Duration::from_secs(2);

/// How many of a program's last peaks are kept (see [Estimates](#estimates)).
pub const KEPT_PEAKS: usize = 4;

/// How long a waiting program first waits before it looks again, doubled
/// each time, and the longest it waits.
const POLL_FIRST: Duration = Duration::from_millis(2);

const POLL_MOST: Duration = Duration::from_millis(50);

/// The budget's limits (see [The budget](#the-budget)).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Limits {
    /// How many programs may run at once.
    pub jobs: usize,
    /// How many bytes they may be expected to use at once.
    pub memory: u64,
    /// What a program of a kind never metered is expected to use, in bytes.
    pub unseen: u64,
}

impl Limits {
    /// The limits of a machine of `cores` processors and `ram` bytes of
    /// memory: each as the environment's `PEKIT_MACHINE_JOBS`,
    /// `PEKIT_MACHINE_MEMORY` and `PEKIT_MACHINE_UNSEEN` say, `var` reading
    /// it; else as the machine's configuration, `file`, says, by its keys
    /// `jobs`, `memory` and `unseen`; else a job for each processor, three
    /// quarters of its memory, and [`DEFAULT_UNSEEN`].
    ///
    /// # Errors
    ///
    /// A setting that is no count or no size, or a line of `file` that sets
    /// nothing it knows.
    pub fn of_machine(
        var: &dyn Fn(&str) -> Option<String>,
        file: Option<&str>,
        (cores, ram): (usize, u64),
    ) -> Result<Self, String> {
        let mut limits = Self {
            jobs: cores.max(1),
            memory: (ram / 4 * 3).max(UNIT),
            unseen: DEFAULT_UNSEEN,
        };
        for (number, line) in file.unwrap_or_default().lines().enumerate() {
            let line = line.split('#').next().unwrap_or_default().trim();
            if line.is_empty() {
                continue;
            }
            let set = line
                .split_once('=')
                .ok_or_else(|| "it sets nothing: write `KEY = VALUE`".to_owned())
                .and_then(|(key, value)| limits.set(key.trim(), value.trim()));
            set.map_err(|why| {
                format!("line {} of the machine's configuration: {why}", number + 1)
            })?;
        }
        for (name, key) in [
            ("PEKIT_MACHINE_JOBS", "jobs"),
            ("PEKIT_MACHINE_MEMORY", "memory"),
            ("PEKIT_MACHINE_UNSEEN", "unseen"),
        ] {
            if let Some(value) = var(name).filter(|value| !value.trim().is_empty()) {
                limits
                    .set(key, value.trim())
                    .map_err(|why| format!("`{name}`: {why}"))?;
            }
        }
        Ok(limits)
    }

    /// Sets the limit `key` to `value`.
    ///
    /// # Errors
    ///
    /// That `key` is no limit, or `value` is not one.
    fn set(&mut self, key: &str, value: &str) -> Result<(), String> {
        let size = || {
            size_of(value).filter(|&size| size > 0).ok_or_else(|| {
                format!(
                    "`{value}` is no size: write a number of bytes, such as `24GiB` or `512MiB`"
                )
            })
        };
        match key {
            "jobs" => {
                self.jobs =
                    value.parse().ok().filter(|&jobs| jobs > 0).ok_or_else(|| {
                        format!("`{value}` is no number of jobs: write one above 0")
                    })?;
            }
            "memory" => self.memory = size()?,
            "unseen" => self.unseen = size()?,
            other => {
                return Err(format!(
                    "`{other}` is no limit: write `jobs`, `memory` or `unseen`"
                ));
            }
        }
        Ok(())
    }

    /// How many [`UNIT`]s of memory the budget has: at least one.
    #[must_use]
    pub fn units(&self) -> usize {
        usize::try_from(self.memory / UNIT)
            .unwrap_or(usize::MAX)
            .max(1)
    }

    /// How many units a program expected to use `bytes` takes: at least
    /// one, and at most all the budget has.
    #[must_use]
    pub fn units_for(&self, bytes: u64) -> usize {
        usize::try_from(bytes.div_ceil(UNIT))
            .unwrap_or(usize::MAX)
            .clamp(1, self.units())
    }
}

/// How much memory the machine has, by `/proc/meminfo`, in bytes, if it
/// says.
#[must_use]
pub fn machine_memory() -> Option<u64> {
    let info = fs::read_to_string("/proc/meminfo").ok()?;
    let line = info.lines().find(|line| line.starts_with("MemTotal:"))?;
    let kib: u64 = line.split_whitespace().nth(1)?.parse().ok()?;
    kib.checked_mul(1024)
}

/// The budget's directory: `pekit/budget` under `$XDG_RUNTIME_DIR`, which
/// the system empties when the user's last session ends, if it is set;
/// else `budget` under the cache directory `cache`.
#[must_use]
pub fn budget_dir(var: &dyn Fn(&str) -> Option<String>, cache: &Path) -> PathBuf {
    match var("XDG_RUNTIME_DIR").filter(|dir| !dir.is_empty()) {
        Some(dir) => PathBuf::from(dir).join("pekit/budget"),
        None => cache.join("budget"),
    }
}

/// The budget, as one process uses it (see [The budget](#the-budget)).
#[derive(Debug)]
pub struct Budget {
    /// Its directory.
    dir: PathBuf,
    /// Its limits.
    limits: Limits,
    /// Where programs' peaks are kept (see [Estimates](#estimates)).
    peaks: PathBuf,
    /// The program that meters each program of a step, pekit's own, if
    /// they are metered (see [Metering](crate::meter#metering)).
    meter: Option<PathBuf>,
    /// The order this process's programs wait in.
    queue: Mutex<Queue>,
    /// Told whenever a program of this process starts or ends.
    turn: Condvar,
}

/// The order a process's programs wait for the budget in: each takes a
/// ticket, and only the one being served looks.
#[derive(Debug, Default)]
struct Queue {
    /// The next ticket.
    next: u64,
    /// The ticket being served.
    serving: u64,
}

/// What a program holds of the budget while it runs: let go of when it is
/// dropped.
#[derive(Debug)]
pub struct Held<'a> {
    /// The budget.
    budget: &'a Budget,
    /// The lock files it holds.
    slots: Vec<File>,
    /// How long it waited for them.
    waited: Duration,
}

impl Held<'_> {
    /// How long it waited for what it holds.
    #[must_use]
    pub fn waited(&self) -> Duration {
        self.waited
    }

    /// How many lock files it holds.
    #[must_use]
    pub fn slots(&self) -> usize {
        self.slots.len()
    }
}

impl Drop for Held<'_> {
    fn drop(&mut self) {
        // Closing them lets go of them.
        self.slots.clear();
        self.budget.turn.notify_all();
    }
}

impl Budget {
    /// The budget whose directory is `dir`, of `limits`, its programs'
    /// peaks kept in `peaks`, each program of a step metered by the program
    /// `meter` if there is one: made if it is not there.
    ///
    /// # Errors
    ///
    /// That its directory cannot be made.
    pub fn open(
        dir: &Path,
        limits: Limits,
        (peaks, meter): (&Path, Option<PathBuf>),
    ) -> io::Result<Self> {
        fs::create_dir_all(dir)?;
        Ok(Self {
            dir: dir.to_path_buf(),
            limits,
            peaks: peaks.to_path_buf(),
            meter,
            queue: Mutex::new(Queue::default()),
            turn: Condvar::new(),
        })
    }

    /// Its directory.
    #[must_use]
    pub fn dir(&self) -> &Path {
        &self.dir
    }

    /// Its limits.
    #[must_use]
    pub fn limits(&self) -> Limits {
        self.limits
    }

    /// Where programs' peaks are kept.
    #[must_use]
    pub fn peaks(&self) -> &Path {
        &self.peaks
    }

    /// The program that meters each program of a step, if they are metered.
    #[must_use]
    pub fn meter(&self) -> Option<&Path> {
        self.meter.as_deref()
    }

    /// Waits until a program expected to use `bytes` may run, in the order
    /// this process's programs asked (see [The budget](#the-budget)): what
    /// it then holds. Where the budget's files cannot be opened, it runs
    /// at once, holding nothing: the budget paces programs, and never fails
    /// one.
    pub fn admit(&self, bytes: u64) -> Held<'_> {
        let began = Instant::now();
        let units = self.limits.units_for(bytes);
        let mut queue = self.queue();
        let ticket = queue.next;
        queue.next += 1;
        let (mut waiting, mut gate) = (None, None);
        let mut pause = POLL_FIRST;
        loop {
            if queue.serving == ticket {
                match self.try_take(units, gate.is_some()) {
                    Ok(Some(slots)) => {
                        queue.serving += 1;
                        drop(queue);
                        self.turn.notify_all();
                        return Held {
                            budget: self,
                            slots,
                            waited: began.elapsed(),
                        };
                    }
                    Ok(None) => {}
                    Err(_) => {
                        queue.serving += 1;
                        drop(queue);
                        self.turn.notify_all();
                        return Held {
                            budget: self,
                            slots: Vec::new(),
                            waited: began.elapsed(),
                        };
                    }
                }
                if waiting.is_none() {
                    waiting = self.locked("waiting", false).ok();
                }
                if gate.is_none() && began.elapsed() >= STARVED {
                    // Held until this program starts, without this
                    // process's queue, which its other programs end by.
                    drop(queue);
                    gate = self.locked("gate", true).ok();
                    queue = self.queue();
                    continue;
                }
            }
            queue = self
                .turn
                .wait_timeout(queue, pause)
                .unwrap_or_else(PoisonError::into_inner)
                .0;
            pause = (pause * 2).min(POLL_MOST);
        }
    }

    /// This process's queue.
    fn queue(&self) -> MutexGuard<'_, Queue> {
        self.queue.lock().unwrap_or_else(PoisonError::into_inner)
    }

    /// A job's lock file and `units` of memory's, all free now, held; or
    /// none, if they are not all free, or another program holds the gate,
    /// unless this one does, `gated`.
    ///
    /// # Errors
    ///
    /// That a lock file cannot be opened or locked.
    fn try_take(&self, units: usize, gated: bool) -> io::Result<Option<Vec<File>>> {
        let _gate = if gated {
            None
        } else {
            match self.try_locked("gate", false)? {
                Some(gate) => Some(gate),
                None => return Ok(None),
            }
        };
        let Some(mut slots) = self.free("job", self.limits.jobs, 1)? else {
            return Ok(None);
        };
        let Some(memory) = self.free("memory", self.limits.units(), units)? else {
            return Ok(None);
        };
        slots.extend(memory);
        Ok(Some(slots))
    }

    /// A job's lock file, free now, held, for a program's jobserver to lend
    /// (see [Jobservers](crate::meter#jobservers)): none if none is, or a
    /// program holds the gate.
    ///
    /// # Errors
    ///
    /// That a lock file cannot be opened or locked.
    pub fn try_job(&self) -> io::Result<Option<File>> {
        if self.try_locked("gate", false)?.is_none() {
            return Ok(None);
        }
        Ok(self
            .free("job", self.limits.jobs, 1)?
            .and_then(|mut slots| slots.pop()))
    }

    /// Whether a program of any process waits for the budget.
    ///
    /// # Errors
    ///
    /// That `waiting` cannot be opened or locked.
    pub fn others_wait(&self) -> io::Result<bool> {
        Ok(self.try_locked("waiting", true)?.is_none())
    }

    /// `wanted` of the lock files `<kind>-<n>`, for `n` below `count`, free
    /// now, held: none if fewer are. Each process looks from a place of its
    /// own, so that processes seldom try the same files.
    ///
    /// # Errors
    ///
    /// That a lock file cannot be opened or locked.
    fn free(&self, kind: &str, count: usize, wanted: usize) -> io::Result<Option<Vec<File>>> {
        let start = usize::try_from(std::process::id()).unwrap_or_default();
        let mut held = Vec::with_capacity(wanted);
        for offset in 0..count {
            if held.len() == wanted {
                break;
            }
            let name = format!("{kind}-{}", (start + offset) % count);
            if let Some(file) = self.try_locked(&name, true)? {
                held.push(file);
            }
        }
        Ok((held.len() == wanted).then_some(held))
    }

    /// The lock file `name`, opened, made if it is not there.
    ///
    /// # Errors
    ///
    /// That it cannot be.
    fn file(&self, name: &str) -> io::Result<File> {
        File::options()
            .create(true)
            .truncate(false)
            .write(true)
            .open(self.dir.join(name))
    }

    /// The lock file `name`, held, `exclusive` or shared, waiting for it.
    ///
    /// # Errors
    ///
    /// That it cannot be opened or locked.
    fn locked(&self, name: &str, exclusive: bool) -> io::Result<File> {
        let file = self.file(name)?;
        if exclusive {
            file.lock()?;
        } else {
            file.lock_shared()?;
        }
        Ok(file)
    }

    /// The lock file `name`, held, `exclusive` or shared, if it may be now.
    ///
    /// # Errors
    ///
    /// That it cannot be opened or locked.
    fn try_locked(&self, name: &str, exclusive: bool) -> io::Result<Option<File>> {
        let file = self.file(name)?;
        let locked = if exclusive {
            file.try_lock()
        } else {
            file.try_lock_shared()
        };
        match locked {
            Ok(()) => Ok(Some(file)),
            Err(TryLockError::WouldBlock) => Ok(None),
            Err(TryLockError::Error(error)) => Err(error),
        }
    }

    /// What the program `program`, a file name, of the step `step` is
    /// expected to use, in bytes (see [Estimates](#estimates)).
    #[must_use]
    pub fn estimate(&self, step: &str, program: &str) -> u64 {
        let [own, kind] = peak_files(&self.peaks, step, program);
        let peak = read_peaks(&own)
            .into_iter()
            .max()
            .or_else(|| read_peaks(&kind).into_iter().max());
        peak.map_or(self.limits.unseen, |peak| peak.saturating_add(peak / 4))
    }
}

/// The files the peaks of the program `program`, a file name, of the step
/// `step` are kept in, under `peaks`: the step's own, and its kind's (see
/// [Estimates](#estimates)).
#[must_use]
pub fn peak_files(peaks: &Path, step: &str, program: &str) -> [PathBuf; 2] {
    let kind = step.rsplit_once('/').map_or(step, |(kind, _)| kind);
    let own = Digest::of(format!("pekit-peak-1\0{step}\0{program}").as_bytes()).hex();
    let kind = Digest::of(format!("pekit-peak-kind-1\0{kind}\0{program}").as_bytes()).hex();
    [peaks.join(own), peaks.join(format!("kind-{kind}"))]
}

/// The peaks kept in `file`, oldest first: none if it cannot be read.
fn read_peaks(file: &Path) -> Vec<u64> {
    fs::read_to_string(file)
        .unwrap_or_default()
        .split_whitespace()
        .filter_map(|peak| peak.parse().ok())
        .collect()
}

/// Keeps `peak`, in bytes, as the last peak of the program `program`, a
/// file name, of the step `step`, under `peaks` (see
/// [Estimates](#estimates)), for the step and its kind, with the
/// [`KEPT_PEAKS`] before it at most. Each file is replaced whole, so that a
/// reader never sees half of one.
///
/// # Errors
///
/// That a file cannot be written.
pub fn keep_peak(peaks: &Path, (step, program): (&str, &str), peak: u64) -> io::Result<()> {
    fs::create_dir_all(peaks)?;
    for file in peak_files(peaks, step, program) {
        let mut kept = read_peaks(&file);
        kept.push(peak);
        let from = kept.len().saturating_sub(KEPT_PEAKS);
        let text: Vec<String> = kept[from..].iter().map(u64::to_string).collect();
        let beside = file.with_extension(format!("new-{}", crate::sweep::token()));
        fs::write(&beside, text.join("\n") + "\n")?;
        fs::rename(&beside, &file)?;
    }
    Ok(())
}

/// The file name of the program `program` runs, as its peaks are kept by.
#[must_use]
pub fn program_name(program: &str) -> &str {
    program.rsplit('/').next().unwrap_or(program)
}
