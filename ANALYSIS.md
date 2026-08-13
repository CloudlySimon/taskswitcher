# Task Switcher — Kiosk Reliability Analysis (merged)

**Date:** 2026-08-14
**Subject:** `cmd/taskswitcher-toolbar` (the shipped kiosk binary), `internal/winutil`, `internal/config`
**Reported symptom:** the toolbar freezes for many seconds after a button press; occasionally long enough that an external watchdog terminates the process.

> **Provenance.** This document merges two independent source reviews of the same tree: a
> *blocking-call* analysis (what makes one click take seconds) and a *state-machine and policy*
> analysis (what happens to the system once a click is slow). Findings are tagged **[A]**, **[B]**,
> or **[A+B]** by origin so each can be traced back. Every claim below has been re-verified against
> the source at the cited line.

---

## 1. Executive summary

Keeping this as a small native Windows program is the right call. The problem is not the choice of
technology; it is that the launch path performs **unbounded blocking work**, and that the
coordination logic around that work **amplifies a slow operation into duplicate launches** instead
of containing it.

Two distinct failure modes are in play, and conflating them has made the system hard to reason
about:

| Failure mode | Question it answers | Primary causes |
|---|---|---|
| **Acute stall** | "Why does one click freeze for seconds?" | F1, F2, F6, F7 |
| **Progressive degradation** | "Why does the kiosk get worse over hours until something kills it?" | F3, F4, F5, F8 |

The acute stall is caused by window discovery that is quadratic in the number of open windows
(**F1**) and that calls a cross-process API which blocks indefinitely with no timeout (**F2**).

The progressive degradation is caused by a chain of three defects that compound: the toolbar
matches only the *first* PID with a given executable name (**F4**), which reports "not running" for
multi-process applications; the `switchOnly` policy that would prevent a launch on that false
negative **is never enforced** (**F5**); and the three-second "watchdog" clears the busy flag
without cancelling the in-flight operation, **authorising a second concurrent launch over the top
of the first** (**F3**). The example configuration ships `msedge` with `--kiosk` arguments and
`switchOnly: true`, so this chain terminates in repeated kiosk-browser launches.

Separately, **F8** is a hard process-death mechanism: `windows.NewCallback` is called per
invocation against a fixed 2000-entry table that is never freed, so a long-lived kiosk eventually
panics outright.

### On what can and cannot be proven

The repository contains **no toolbar runtime log, no watchdog configuration, no hang dump, and no
trace from an affected kiosk**. Source review can therefore establish that these paths are unsafe;
it cannot establish which one caused any particular field incident. Section 8 sets out how to
close that gap.

One clarification that matters: **the code labelled "watchdog" does not kill anything.** It clears
a boolean after three seconds (`main.go:544-551`). The process-terminating watchdog described in
the incident is external to this repository and its product, configuration, and threshold are
unknown.

---

## 2. Evidence base

**Verified during this review:**

- The toolbar, the simple command, and the shared packages build for `windows/amd64`.
- **`cmd/taskswitcher-ui` does not compile** — `u.mw` and `u.label` are used throughout
  (`cmd/taskswitcher-ui/main.go:96,102,120,152,…`) but are absent from the `UI` struct
  (`:19-23`).
- There are **no tests** anywhere in the tree.
- `debugLog` is **defined and never called** (`main.go:163`; zero call sites).
- `IsSwitchOnly` is **defined and never called** (`config/config.go:44`; zero call sites).
- Neither `Process.Release()` nor `Wait()` appears anywhere in the tree.
- `NewCallback` appears at four sites: `main.go:305` and `:735` are correctly one-shot;
  `winutil_windows.go:92` and `:156` are **per-invocation**.
- `AppVersion = "0.1.4-dev"` (`main.go:28`) vs `version.txt` = `0.1.2-dev`.
- The toolbar `.exe` at repository root and the one inside `taskswitcher-toolbar.zip` have
  **different sizes and checksums**.
- All history is a single commit, so no change can be correlated with a field release.

**Not evidence:** `nelwa/nelwa.log` is from a different program (its own web view and network
scanner). It shows restarts and one port-bind conflict, but attributing those to Task Switcher
would be speculation.

---

## 3. Findings

Ordered by explanatory power for the reported symptom, not by abstract severity.

### F1 — Window discovery is quadratic in open windows · **Acute stall** · Certain · [A+B]

**Evidence:** `internal/winutil/winutil_windows.go:149-181`. `FindProcessWindowByExeAndTitleContains`
enumerates every visible top-level window, and for **each one** calls `getExeNameByPid`
(`:125-147`), which creates and walks a **complete system-wide Toolhelp process snapshot**.

For `W` visible top-level windows and `P` processes, the miss path costs roughly `W` full snapshot
creations and up to `W × P` entry comparisons. `simpleLaunch` then calls
`FindFirstWindowByProcessName` for **another** snapshot (`main.go:889`), and the post-launch retry
repeats the entire exercise 1.5 seconds later (`main.go:938-949`).

A typical kiosk has 60–150 visible top-level windows and 120–250 processes.
`CreateToolhelp32Snapshot` is a synchronous kernel-side walk, commonly 5–50 ms, and materially
worse when an AV filter driver hooks process enumeration — near-universal on managed fleets.

> **100 windows × 30 ms ≈ 3 seconds, per button press, before any launching happens.**

**Fix:** one snapshot per refresh → build `pid → exe` and `exe → []pid` → **one** `EnumWindows`
pass consulting the maps. O(P + W) instead of O(W × P). Better: cache the resolved `HWND` per task
and revalidate with `IsWindow` on each click, rescanning only on cache miss. A `SetWinEventHook`
index is the eventual form; bounded periodic rescan is an acceptable first version.

### F2 — `GetWindowTextW` cross-process is a blocking `SendMessage` with no timeout · **Acute stall** · Certain · [A]

**Evidence:** `internal/winutil/winutil_windows.go:114-123`, called from the enumeration callback
at `:171`.

For a window owned by **another process**, USER32 cannot read the title directly. It implements
`GetWindowTextW` (and `GetWindowTextLengthW`) as a synchronous `SendMessage(hwnd, WM_GETTEXT, …)`
that blocks the calling thread until the *owning* thread dispatches it. There is no timeout and no
cancellation.

This is the **only mechanism in the codebase that can block indefinitely** rather than merely
slowly. Everything else in this document is slow; this one is unbounded. It is therefore the
strongest single candidate for stalls long enough to trip an external watchdog.

Note the irony: the application most likely to be mid-startup and not pumping its message queue is
the Crestron XPanel you are trying to focus.

**Fix:** never call `GetWindowTextW` cross-process. Use
`SendMessageTimeoutW(…, WM_GETTEXT, …, SMTO_ABORTIFHUNG|SMTO_BLOCK, 200, …)`, or
`InternalGetWindowText`, which reads the title kernel-side without messaging the owner at all.

### F3 — The 3-second timeout permits overlapping work but does not cancel it · **Degradation** · Certain · [B]

**Evidence:** `main.go:544-551` sets `processing = false` after three seconds. The original
goroutine keeps running `simpleLaunch` — it has no context, deadline, cancellation, or generation
token. A queued command may then start immediately (`main.go:553-569`).

This is not a stall detector. It is an **overlap authoriser**:

- A slow snapshot or shell activation gets a second copy started over the top of it.
- Repeated intervals plus repeated user taps produce duplicate application launches.
- When the first goroutine finally returns, its `defer` sets `processing = false`
  (`main.go:847-849`) **even though a newer command is still running** — so the flag no longer
  describes reality, and the *next* command overlaps too.
- If the original slowness was caused by system pressure, adding concurrent copies makes recovery
  strictly less likely.

This explains the "pause, then a burst of activity" signature, and it is the mechanism by which a
single slow operation escalates into sustained degradation.

**Fix:** delete `processing`, `processingSince`, and `pendingTask`. Replace with one command queue
owned by one worker goroutine — a capacity-one channel with an explicit coalescing policy (for a
kiosk, "latest requested destination wins"). Every request carries a monotonically increasing ID; a
late result may be *logged* but must never overwrite state belonging to a newer request. A timeout
may **report** an operation as slow; it must never authorise overlap unless the previous operation
was genuinely cancelled or isolated in a killable process.

### F4 — First-PID matching does not find the application's window · **Degradation** · Certain · [B]

**Evidence:** `findFirstPidByName` returns on the first executable-name match
(`winutil_windows.go:66-87`); `FindFirstWindowByProcessName` then searches **only that PID**
(`:57-63`). The comparison `exe == name || exe == name+".exe"` is also **case-sensitive**, though
Windows executable matching is not.

Multi-process applications routinely run many PIDs under one executable name. The first entry in a
snapshot is frequently a GPU, renderer, or utility process with no visible top-level window — so a
perfectly good window owned by a later PID is never considered.

Result: false "not running", failure to switch, duplicate launches, and users tapping repeatedly.
`config-example.json:19` configures **`msedge`**, for which first-PID selection is close to
worst-case.

**Fix:** enumerate **all** matching PIDs and **all** candidate windows, then apply task matching in
priority order: valid cached handle → executable + title/class predicate → any eligible window for
any matching PID → launch policy. Normalise with `strings.EqualFold` and accept names with or
without `.exe` consistently.

### F5 — `switchOnly` is silently ignored · **Degradation** · Certain · [B]

**Evidence:** `Task.IsSwitchOnly` is defined at `internal/config/config.go:42-49` and set to `true`
at `config-example.json:21`. **`simpleLaunch` never calls it.** After a lookup miss it proceeds
straight to launching the configured target (`main.go:896-925`).

This is the safety valve that should stop F4's false negatives from becoming real damage — and it
does not exist at runtime.

**F4 + F5 + F3 compose into the degradation loop:**

```
first msedge.exe PID is a renderer with no window   (F4)
        ↓
lookup reports "not running"
        ↓
switchOnly:true is never checked                    (F5)
        ↓
a second kiosk Edge (--kiosk, full args) is launched
        ↓
system slows; next lookup is slower still           (F1, F2)
        ↓
3s timeout clears the flag mid-flight               (F3)
        ↓
a third launch overlaps the second …
```

**Fix:** enforce launch policy *before* either `exec.Command` or `ShellExecute`. A switch-only miss
must return a distinct, visible outcome ("DSP is not running") and never start a process.

### F6 — Synchronous logging can block the message pump via the logger mutex · **Acute stall** · High · [A+B]

**Evidence:** in debug mode the standard logger writes directly to an unbuffered file
(`main.go:212-238`). `wndProc` and the button subclass call `log.Printf` during command and timer
processing (`main.go:477-595`, `:601-637`); the `default:` branch (`:591-595`) logs **nearly every
window message**. A Go ticker logs through the same logger every second (`:409-437`).

The mechanism is the **shared mutex**, not the write speed: `log.Logger` serialises all writers
behind one lock. If a worker goroutine blocks mid-write — slow disk, AV scanning the log file,
network-mounted working directory — it **holds that lock**, and the next `log.Printf` from the UI
thread blocks acquiring it. The message pump stops. This is a direct, provable path from a slow
filesystem to a frozen UI.

Two corrections to earlier drafts of this analysis:

- `debugLog` (`main.go:163`), which calls `Sync()` per line, is **unused**. It is not a contributing
  cause today. It must also **not** be adopted as a fix — forcing every record to disk would
  enlarge the blocking window.
- Logs append with **no rotation** (`main.go:215`). On a kiosk this is a disk-fill risk.
  (`nelwa/nelwa.log` is already 74,186 lines.)

**Fix:** never write to disk from the UI thread. Push compact events to a bounded channel drained
by a dedicated writer; drop-and-count low-priority heartbeats when full; rotate by size; always
retain the last slow-operation and UI-latency records. Enable low-volume operational logging **by
default** rather than discarding all evidence unless `-debug` is passed.

### F7 — `ShellExecuteW` on an arbitrary goroutine thread, with the toolbar as owner · **Acute stall** · High · [A+B]

**Evidence:** `main.go:953-1000`, invoked from the goroutine spawned at `main.go:841`.

Four compounding problems:

1. **No COM apartment.** Go schedules the goroutine onto an arbitrary OS thread that has never
   called `CoInitializeEx(nil, COINIT_APARTMENTTHREADED)`. Resolving a `.c3p` association goes
   through the shell association layer and possibly DDE activation; on an uninitialised thread this
   takes a slow path and can block for many seconds.
2. **No thread affinity.** Without `runtime.LockOSThread`, the goroutine may migrate mid-call.
   Shell and COM calls assume a stable thread.
3. **`u.hwnd` is passed as the owner window** (`main.go:986`). This lets the shell send messages to
   — and take modal ownership of — the toolbar's UI thread.
4. **No handle, no timing, no observability.** `ShellExecuteW` returns only a legacy status code.

Point 3 explains a symptom the code already works around: the WM_TIMER sweep at `main.go:571-581`
that hunts for buttons "Windows can disable after certain interactions" and re-enables them.
**Owner-window modality is precisely how a button gets disabled behind your back.** That workaround
is a fingerprint of this bug.

**Fix:** `ShellExecuteExW` with `SEE_MASK_NOASYNC | SEE_MASK_FLAG_NO_UI`, owner `NULL`, on the
dedicated `LockOSThread` + STA worker thread; capture the process handle when Windows supplies one;
time the call; never overlap activation of the same task. If the `.c3p` association can be resolved
at install time, configuring the owning executable and arguments directly is more deterministic
than invoking a file association at all.

### F8 — `NewCallback` leak against a fixed 2000-entry table · **Process death** · Certain · [A]

**Evidence:** `winutil_windows.go:92` and `:156` call `windows.NewCallback` **inside** the
function — once per invocation. (By contrast `main.go:305` and `:735` are correctly one-shot;
`:735` is guarded by `if btnWndProcPtr == 0`.)

The Go runtime holds callback trampolines in a fixed-size table (2000 entries) and **never releases
them**. Each button press consumes one or two slots. At a few hundred presses per day, a kiosk
panics within days to weeks with *"too many callback functions"* — no recovery, no useful log line.

This is the one mechanism identified anywhere in this analysis that terminates the process **on its
own**, without help from an external watchdog. It deserves attention precisely because the observed
symptom includes unexplained process death.

**Fix:** construct each callback exactly once (package `init` or `sync.Once`) and pass per-call
state through the `lParam` argument that `EnumWindows` already provides for this purpose.

### F9 — The in-process heartbeat cannot recover what it detects · **Design** · Certain · [A+B]

**Evidence:** the UI heartbeat is a `WM_TIMER` whose receipt updates `lastTimerAt`
(`main.go:527-543`). When the Go ticker observes a gap it calls `SetTimer` again and posts another
`WM_TIMER` (`main.go:393-437`).

A window timer message is handled by **the very thread being monitored**. If that thread is
blocked, posting more messages to it cannot make it run. Worse, while the block persists the ticker
adds another synthetic message every second, so when processing finally resumes, a backlog of stale
diagnostics competes with real user input. Calling this a watchdog implies a recovery capability it
does not have.

The diagnosis is also wrong on its own terms: `WM_TIMER` never stopped being *generated*; it stopped
being *dispatched*. Re-arming `SetTimer` addresses nothing.

**Fix:** the in-process monitor should **only record UI latency**. An out-of-process supervisor
decides restart policy, and must capture a dump or at minimum the thread wait chain **before**
terminating — otherwise every restart destroys the evidence needed to find the blocking call.
Health should distinguish four states: process alive; UI thread dispatching; worker idle/busy plus
operation age; target launch/focus result.

### F10 — Click handling is a stack of eight workarounds · **Design** · Certain · [A+B]

Native buttons already notify their parent with `BN_CLICKED`. The code nonetheless subclasses each
button and posts a **second, synthetic** `BN_CLICKED` on `WM_LBUTTONUP` (`main.go:601-632`) — then
adds a 90 ms de-duplication map to suppress the duplicate it just created (`:512`), then a 200 ms
debounce (`:797`), then a 120 ms recursive `AfterFunc` re-fire because the debounce eats real
clicks (`:805`).

| Mechanism | Location | Works around |
|---|---|---|
| Synthetic `BN_CLICKED` on `WM_LBUTTONUP` | `main.go:616-629` | clicks not arriving |
| 90 ms duplicate suppression | `main.go:512` | the synthesis above |
| 200 ms per-task debounce | `main.go:797` | users tapping a frozen UI |
| `pendingTask` single-slot queue | `main.go:826` | clicks lost while blocked |
| 120 ms delayed re-fire | `main.go:805` | the debounce eating a real click |
| 3 s stuck-state clear | `main.go:546` | goroutines never finishing (**F3**) |
| `SetTimer` re-arm | `main.go:426` | WM_TIMER "stopping" (**F9** — it isn't) |
| Button re-enable sweep | `main.go:571` | owner-window modality (**F7**) |

These interact: the debounce fights the pending queue, the delayed re-fire can race the timer
drain, the de-dup window is tuned against the synthesis. That is why behaviour differs between
machines. It also makes it impossible to tell whether a field click problem originates in Windows
input, application state, or the custom subclass.

**Fix:** remove the subclassing and synthetic commands entirely; handle native `BN_CLICKED` once.
Visually mark only the requested button while the serialized worker runs. Coalesce identical queued
destinations instead of using time-based recursive callbacks. **All eight go away** once F1–F7 are
fixed.

### F11 — Focus failures are silent · **Correctness** · High · [A+B]

**Evidence:** `ShowWindow` and `SetForegroundWindow` return booleans
(`winutil_windows.go:37-45`); `bringWindowToFront` discards both (`main.go:857-864`).

Windows' foreground lock only permits `SetForegroundWindow` to succeed from a process that
currently owns the foreground **and is servicing the user input that requested it**. By the time
this runs — on a background goroutine, seconds after the click, after F1/F2 have burned the input
timeout — the app has forfeited that right. The call fails, returns 0, the result is discarded, and
Windows flashes the taskbar button instead of raising the window. The 1500 ms retry at
`main.go:938` is a workaround for this.

**Fix:** attempt the foreground switch on the **UI thread inside the click handler**, while the app
still holds foreground rights. Verify the foreground window afterwards. Report *success*,
*retryable failure*, *not running*, and *launch failure* as distinct outcomes with a short status on
the button. A user should never have to tap repeatedly to learn whether the request was accepted.

### F12 — Launched process handles are never released · **Resource leak** · Certain · [B]

**Evidence:** `cmd.Start()` at `main.go:908-932` with no corresponding `Wait` or `Process.Release`
anywhere in the tree. A kiosk that launches and relaunches targets for weeks retains OS process
resources until finalization happens to run.

**Fix:** for an intentionally unsupervised child, call `cmd.Process.Release()` after a successful
start. If the toolbar should supervise a child, hold explicit child state and reap it with `Wait` in
a dedicated goroutine. Do not mix the two ownership models.

### F13 — `unsafe.Pointer` → `uintptr` conversions that Go does not guarantee · **Latent** · Medium · [A]

**Evidence:** throughout — e.g. `winutil_windows.go:94`, `:121`; `main.go:348`, `:987-990`:

```go
procGetWindowThreadProcessId.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&winpid)))
```

Go's `unsafe` rules only permit `uintptr(unsafe.Pointer(x))` when the conversion appears **in the
argument list of `syscall.Syscall` itself**. Routing it through the variadic `LazyProc.Call` breaks
that guarantee: the garbage collector may move or collect the object between conversion and call.

Rare in practice — but "rare random memory corruption on a long-running kiosk" is a second
plausible explanation for unexplained termination, and it leaves nothing in the logs.

**Fix:** prefer typed wrappers from `golang.org/x/sys/windows`; where none exists, write
`//go:uintptrescapes` wrapper functions. Build and soak with `-race`.

### F14 — Deployment provenance is unresolvable · **Operational** · Certain · [B]

- Source declares `0.1.4-dev`; `version.txt` says `0.1.2-dev`.
- The checked-in toolbar `.exe` reports `0.1.4-dev` and postdates `version.txt`.
- The `.exe` inside `taskswitcher-toolbar.zip` has a **different size and checksum** from the
  standalone one.
- `main.go`'s working-tree mtime is newer than both artifacts (though mtimes cannot identify which
  source produced a binary).
- All history is one commit.

It is therefore **unknown** whether a given kiosk runs the archive binary, the standalone binary, a
build from current source, or something else. *A correct source fix is worthless if deployment can
silently select an older artifact.*

**Fix:** build in CI or one scripted release command; derive one version from the commit and embed
it at link time; emit a checksum manifest; stop tracking mutable binaries at repository root. Log
version, executable path, config path, and config hash at startup, and expose the same through a
diagnostic flag. Keep rollback packages immutable and uniquely named.

### F15 — Four divergent entry points · **Maintenance** · Certain · [A+B]

| Binary | Lines | Toolkit | State |
|---|---|---|---|
| `cmd/taskswitcher.go` | 78 | Fyne | prototype; hardcoded process names; spawns an unbounded sleeping goroutine on every keyboard click |
| `cmd/taskswitcher-simple` | 149 | console | CLI/interactive |
| `cmd/taskswitcher-ui` | 237 | walk + raw Win32 | **does not compile** |
| `cmd/taskswitcher-toolbar` | 1001 | raw Win32 | **shipped** |

`go test ./...` is unusable as a release gate while one package does not build. Nothing in the repo
states which binary is deployed. Three `.exe` files, a `.zip`, and `nelwa/nelwa.log` (~10 MB total)
are tracked in git.

**Fix:** declare `cmd/taskswitcher-toolbar` the single supported application; move prototypes to
history or an explicitly excluded `experiments/` tree; drop the Fyne and Walk dependencies if no
production command uses them; add `.gitignore`. A release must fail unless every supported package
builds and its tests pass.

### F16 — Smaller definite issues · [A+B]

- **Config shape.** Two hard-coded slots (`CloudTask`, `WaveFrontTask`) rather than a task list with
  stable IDs. `safeButtonClick` uses *process name* as identity, so two buttons sharing an
  executable with different titles or arguments collide — first config wins.
- **Dead config.** `SystemLabel`, `KeyboardHeight`, and `TextColor` have no effect on the toolbar;
  `applyButtonColors` (`main.go:770`) is an empty placeholder.
- **CWD-relative paths.** The default config path and the debug log path resolve against the process
  working directory (`main.go:208`, `:215`). Startup mechanisms — services, scheduled tasks, shell
  replacements — routinely supply a different one, making behaviour depend on *how* the executable
  was launched.
- **No config validation.** `config.Load` (`config/config.go:50`) accepts unknown fields and
  validates nothing: no check for duplicate IDs, missing paths, invalid position/colour, conflicting
  launch modes, or a missing title requirement on a multi-process target. `main.go:243` responds to
  any error with `log.Fatalf` — a typo yields a kiosk with **no menu at all** and, since logging
  defaults to `io.Discard`, no diagnostic.
- **Discarded API errors.** Nearly every Win32 error is dropped, preventing any distinction between
  *not found*, *access denied*, *invalid handle*, *foreground rejected*, and *API failure*.
- **No single-instance mutex.** A supervisor restart race can leave two topmost toolbars if the old
  process has not actually exited.
- **Console flash.** `hideConsoleWindow` (`main.go:195`) hides a console *after* it has appeared,
  because the binary is linked as a console application. Build with `-ldflags="-H=windowsgui"` and
  delete the function.
- **Stale work area.** The work area is read once at startup (`main.go:322`) and never updated, so
  a resolution change, DPI change, or display hot-plug leaves the toolbar mispositioned until
  restart. Registering properly as a Windows **AppBar** (`SHAppBarMessage` / `ABM_SETPOS`) would
  both fix this and stop maximised applications from overlapping the strip.
- **No tests** of any kind: no unit tests, Windows integration tests, responsiveness thresholds,
  soak tests, or handle-count checks.

---

## 4. Recommended architecture

### 4.1 Stay native

For always-visible kiosk navigation that activates native application windows, the direct Win32
implementation is a sound foundation. A browser-based menu would still require a privileged native
helper to enumerate and focus windows — two components and more failure modes. A large
cross-platform GUI framework adds nothing for two or three buttons.

Windows kiosk/shell configuration remains useful for restricting the account and restarting the
menu, but it does not remove the need for deterministic task matching. Replacing the Windows shell
outright should be considered only if lockdown requirements demand it; it raises recovery and
servicing risk.

**On toolkits specifically:** `lxn/walk` (already in `go.mod`) is worth evaluating — it is a
maintained Win32 wrapper with correct subclassing, ownership, and message-loop handling, so it
would *delete* much of the risk surface rather than repair it. Check its current maintenance status
first. **Fyne is the wrong tool here**: OpenGL rendering on kiosk tablet GPUs, a much larger binary,
slower cold start, and no natural expression of a 44 px always-on-top `WS_EX_TOOLWINDOW` strip.

### 4.2 Four small components

1. **UI thread** — create controls, paint status, accept native click notifications, post commands.
   It must **not** enumerate processes, launch targets, wait on locks held by I/O code, or write to
   disk.
2. **Command worker** — one goroutine owning an input queue, executing switch/launch requests
   serially and publishing immutable results to the UI via `PostMessage` (never `SendMessage`).
   Repeat requests coalesce by task ID. Created with `runtime.LockOSThread()` +
   `CoInitializeEx(COINIT_APARTMENTTHREADED)`.
3. **Window index** — one bounded refresh produces candidate windows for *all* configured tasks.
   Caches validated handles. **Never** takes a process snapshot inside an `EnumWindows` callback.
4. **Diagnostics** — a bounded asynchronous writer recording UI latency, operation timings,
   decisions, Windows error codes, versions, and resource counts. An external supervisor reads a
   minimal liveness signal or performs a timeout-safe window ping.

**The governing rule:** *the UI thread pumps messages; nothing else.*

**Explicit state machine:**

```
idle → requested(taskID) → locating → focusing | launching → verifying → success | failure → idle
```

Every request carries a monotonically increasing ID. A late result may be logged but **must not**
overwrite state belonging to a newer request. Every worker operation carries a
`context.WithTimeout` (~2 s); on expiry it logs and gives up. For a kiosk, *responsive and slightly
wrong* beats *correct and frozen*.

### 4.3 Model configuration around intent

Replace the two named slots with a task list keyed by stable IDs, and collapse the overlapping
booleans (`enabled`, `isSystemExtension`, `switchOnly`) into one explicit launch mode:

```json
{
  "tasks": [
    {
      "id": "embr",
      "label": "EMBR",
      "match": { "executable": "elwa.exe" },
      "launch": { "mode": "executable", "path": "C:\\embr\\elwa\\elwa.exe" }
    },
    {
      "id": "crestron",
      "label": "CRESTRON",
      "match": { "executable": "CrestronXPanel.exe", "titleContains": "Xpanel" },
      "launch": { "mode": "associatedFile", "path": "C:\\embr\\crestron\\GhostDonkeyv1.1.c3p" }
    }
  ]
}
```

`mode` is exactly one of `switchOnly` | `executable` | `associatedFile`. Reject invalid
configuration **before** creating the window, and write the error to the Windows event log or
another guaranteed location. Degrade rather than die: a bad entry disables one button; it does not
kill the kiosk.

---

## 5. Implementation plan

### Phase 1 — stop the clock *(contained; mostly one file)*

Ordered so the acute stall goes first, since that is the reported symptom.

1. **Rewrite window discovery as a single pass** — one snapshot → `pid → exe` map → one
   `EnumWindows`. *(F1)*
2. **Replace `GetWindowTextW` with `SendMessageTimeoutW(WM_GETTEXT, SMTO_ABORTIFHUNG, 200 ms)`** or
   `InternalGetWindowText`. *(F2)*
3. **Hoist `NewCallback` to package init**; pass state via `lParam`. *(F8)*
4. **Hoist the `GetParent` `LazyProc`** out of the enumeration callback (`winutil:101`, `:162`) — a
   fresh `LazyProc` per window forces repeated symbol resolution in the hottest loop.
5. **Match all PIDs, case-insensitively** via `strings.EqualFold`. *(F4)*
6. **Enforce `switchOnly`** before any `exec.Command` or `ShellExecute`. *(F5)*
7. **Remove the 3-second mutation of `processing`**; record a slow-operation event instead. *(F3)*
8. **Add one serialized capacity-one/coalescing worker with request IDs.** *(F3)*
9. **Remove button subclassing, synthetic clicks, and recursive debouncing.** *(F10)*
10. **Move all file logging off the UI thread**; reduce heartbeats to transitions and threshold
    breaches; add rotation. *(F6)*
11. **`Process.Release()`** after every detached start. *(F12)*
12. **Named single-instance mutex.** *(F16)*
13. **Resolve config and log paths relative to the executable** or an explicit ApplicationData
    directory. *(F16)*

Steps 1–2 alone should take the observed stall from seconds to low tens of milliseconds.

### Phase 2 — make it observable and testable

1. Introduce interfaces around window enumeration, focus, process launch, clock, and logging.
2. Unit-test: multi-PID matching, title matching, case normalisation, switch-only misses,
   coalescing, late completion, launch failure.
3. Move `ShellExecuteExW` onto the STA worker with owner `NULL`; capture the handle; time it. *(F7)*
4. Perform the foreground switch on the UI thread; check the return; surface distinct outcomes on
   the button. *(F11)*
5. **Optimistic UI:** on `BN_CLICKED`, set the caption to "Starting…" and disable the button within
   the same frame, on the UI thread. Perceived responsiveness then becomes independent of launch
   speed.
6. Bound every worker operation with `context.WithTimeout`.
7. Time each native operation *separately*, not just the whole button action.
8. Remove the `unsafe.Pointer`→`uintptr` hazards; build with `-race`. *(F13)*
9. Delete or quarantine unsupported entry points; drop unused UI dependencies. *(F15)*
10. Configure the external watchdog to **capture a dump before terminating**, with separate
    thresholds for UI liveness and operation duration. *(F9)*

### Phase 3 — validate on a real kiosk image

Soak with the **same** security software, user policy, Crestron association, browser configuration,
and watchdog as production. Cover:

- both targets stopped, starting, running, minimized, hidden, and crashed;
- many processes and top-level windows;
- rapid alternating taps and touch-generated double input;
- target startup slower than the watchdog threshold;
- disk-full and deliberately slowed log writing;
- an application with multiple same-name PIDs (Edge);
- standby/resume, resolution changes, Explorer/shell restarts;
- 72 hours continuous, sampling process count, goroutine count, handle count, memory, UI heartbeat
  latency, and action latency.

**Acceptance criteria:**

- The toolbar continues dispatching UI messages during *every* task operation.
- A click is visibly acknowledged within **100 ms**.
- Exactly one switch/launch operation is active at a time.
- A `switchOnly` task is **never** launched.
- One physical tap produces at most one accepted request.
- Existing target windows are found across **all** matching PIDs.
- Handle count and goroutine count remain bounded across the soak.
- No supervisor termination occurs; if a fault is injected, a dump and the last operation timeline
  survive it.
- Installed binary checksum, embedded version, and config hash match the release manifest.

---

## 6. Immediate operational mitigations (before any code change)

- **Verify which binary is installed, by checksum.** Do not assume the ZIP and standalone
  executables are equivalent — they are not.
- **Do not run `-debug` continuously in production** until logging is decoupled from the UI thread.
  Use it only for a controlled reproduction while collecting a dump.
- **Configure the external watchdog to capture evidence** and to let the old process fully exit
  before starting another.
- **Do not repeatedly tap a task while it is starting** — the current timeout overlaps launches
  after three seconds.
- **For any task that must never be launched by the toolbar, temporarily remove its
  `processFilePath`** as defence in depth. The `switchOnly` flag alone is ineffective (F5).

---

## 7. Proving it in the field

Source review identifies unsafe paths; it does not capture the actual wait. On an affected kiosk:

1. Record the exact binary checksum, command line, working directory, config, watchdog product and
   configuration, and kill threshold.
2. Enable bounded diagnostic events for UI heartbeat latency and entry/exit timing around: process
   snapshot, window enumeration, `GetWindowText`, `SetForegroundWindow`, process start, and shell
   activation.
3. Configure the supervisor to capture a **full process dump immediately before killing** a
   non-responsive toolbar. Preserve Windows event logs for the same window.
4. Correlate the last UI heartbeat against goroutine stacks and native thread wait chains:

   | Observed wait | Confirms |
   |---|---|
   | UI thread blocked on the logger mutex | **F6** synchronous logging contention |
   | Worker inside Toolhelp / window discovery | **F1** snapshot multiplication |
   | Worker inside `SendMessage`/`WM_GETTEXT` | **F2** cross-process title read |
   | Worker inside shell / DDE activation | **F7** associated-file launch |
   | **Multiple** workers in the same action | **F3** the three-second overlap defect |
   | Panic: *"too many callback functions"* | **F8** callback exhaustion |

5. Re-run the identical workload against the serialized worker and one-pass index. **Retain the
   timings** rather than relying on the absence of a visible freeze.

Cheap confirmations available today, in increasing order of effort: wrap
`FindProcessWindowByExeAndTitleContains` in `time.Since` logging (proves F1+F2 cost directly); log a
counter at each `NewCallback` site and check it climbs monotonically with button presses (proves
F8); and establish whether the external watchdog triggers on *process absent* or on
`IsHungAppWindow` — the latter proves the **UI thread**, not merely a worker, is blocking for >5 s,
which sharpens the diagnosis toward F6 and F7.

Absent a dump or a toolbar log from a real incident, it would be premature to name one exact root
cause. **F1, F3, F4, F5, and F8 are definite correctness defects and should be fixed regardless of
what the dump shows.** F2, F6, and F7 are credible, environment-dependent stall sources that
instrumentation should confirm or rule out.
