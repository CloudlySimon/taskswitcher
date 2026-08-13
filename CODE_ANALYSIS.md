# Task Switcher kiosk reliability analysis

Date: 2026-08-14

## Executive summary

The project is a small Windows toolbar that switches to an existing application window or launches the application when no matching window is found. Keeping this as a small native program is a reasonable approach for this kiosk. The present implementation, however, contains several mechanisms added to compensate for unreliable clicks and slow operations. Those mechanisms can amplify a slow operation into overlapping launches and extra system load.

The source review cannot prove which call caused a particular field incident because the repository contains no toolbar runtime log, watchdog configuration, hang dump, or trace from an affected kiosk. The internal code labelled "watchdog" does not kill the process; it only clears a Boolean after three seconds. The killing watchdog described in the incident is therefore external to the code in this repository.

The strongest source-level candidates for the reported long pauses are:

1. Window discovery is unnecessarily expensive. Title-based discovery enumerates every top-level window and creates a complete process snapshot separately for each visible window. It then performs another process snapshot in the fallback path.
2. The three-second internal watchdog does not cancel a slow operation. It declares the application idle while the old operation is still running, which permits a second copy to start. Completion of either copy can also incorrectly clear the shared state for the other.
3. Debug logging is synchronous and is called from the Windows UI thread. A slow filesystem, antivirus scan, or another goroutine holding the standard logger lock can directly stop message processing.
4. Process matching selects only the first PID with a given executable name. This is incorrect for multi-process applications such as browsers and can be incorrect after an application restart. It can conclude that there is no window and launch duplicates.
5. `switchOnly` exists in configuration but is ignored by the toolbar. A configured switch-only task can therefore be launched, including when matching fails for one of the reasons above.

The recommended direction is not a larger UI framework or a web application. It is a smaller, testable version of the existing native design: keep the Win32 message thread limited to presentation, use one serialized/coalescing command worker, build the process/window index in one pass, represent tasks by stable IDs, and make the watchdog observational rather than allowing it to mutate in-flight work.

## Scope and evidence

The apparent production implementation is [`cmd/taskswitcher-toolbar/main.go`](cmd/taskswitcher-toolbar/main.go). It builds for Windows and the checked-in `taskswitcher-toolbar.exe` identifies itself as that Go package. The analysis also covered shared configuration and Windows helpers, the alternate entry points, checked-in binaries, archive, and the supplied `nelwa` log.

Checks performed:

- The toolbar, simple command, and shared packages compile for `windows/amd64`.
- There are no tests in those packages.
- `cmd/taskswitcher-ui` does not compile because `UI.mw` and `UI.label` are used but are not declared.
- The full source tree was read for blocking operations, timers, goroutines, process creation, window enumeration, shared state, and logging.
- The checked-in executable, archive executable, source version, and `version.txt` were compared.

There is no source for `nelwa.exe` in this repository. Its log describes its own web view and network scanner, not the toolbar message loop, so it cannot confirm a Task Switcher hang. It does show several application restarts and one overlapping-instance port-bind failure, but attributing those events to Task Switcher or its watchdog would be speculation.

## Current runtime path

The toolbar performs the following sequence when a button is clicked:

1. A native button sends `WM_COMMAND` to the toolbar window.
2. `safeButtonClick` applies de-duplication and marks one global `processing` flag.
3. A new goroutine calls `simpleLaunch`.
4. `simpleLaunch` optionally searches windows by executable and title, then searches by the first process with the executable name.
5. It focuses the selected window or starts an executable / invokes `ShellExecute` for an associated file.
6. A window timer updates a UI heartbeat once per second. A Go ticker watches that heartbeat.
7. If an operation has been marked as processing for more than three seconds, the window timer clears the flag and may start the pending task even though the original goroutine is still running.

The expensive launch work is normally off the UI thread, which is good. The main reliability problem is the coordination around that work rather than a deliberate sleep in the UI callback.

## Findings

### Critical: the timeout permits overlapping work but does not cancel it

Evidence: `WM_TIMER` sets `processing = false` after three seconds in `cmd/taskswitcher-toolbar/main.go:544-551`. The original goroutine continues executing `simpleLaunch`; it has no context, cancellation, deadline, or generation token. A pending command may immediately be started at `cmd/taskswitcher-toolbar/main.go:553-569`.

Consequences:

- A slow process snapshot or `ShellExecute` can have another copy started over it.
- Repeated watchdog intervals and taps can launch duplicate applications.
- When the first goroutine eventually returns, its `defer` sets `processing = false` even if a newer command is still running (`cmd/taskswitcher-toolbar/main.go:841-850`).
- The state is no longer a truthful representation of active work, so the next command can overlap too.
- If the slow operation is caused by system pressure, starting more copies makes recovery less likely.

This mechanism is a strong explanation for a pause followed by a burst of activity. It can also cause an external watchdog to see sustained system or UI degradation. It is not itself the watchdog that terminates the executable.

Recommendation: replace `processing`, `processingSince`, and `pendingTask` with one command queue owned by one worker. Use a channel of capacity one and a clear coalescing policy (for a kiosk, "latest requested destination wins" is usually appropriate). A timeout may report an operation as slow, but it must not authorize overlapping work unless the previous operation has actually been cancelled or isolated in a process that can be terminated safely.

### High: title matching has process-snapshot multiplication

Evidence: `FindProcessWindowByExeAndTitleContains` enumerates all visible top-level windows at `internal/winutil/winutil_windows.go:149-180`. For every one, it calls `getExeNameByPid`, which creates and walks a new system-wide Toolhelp process snapshot at `internal/winutil/winutil_windows.go:125-147`.

If there are `W` visible top-level windows and `P` processes, the unsuccessful path is approximately `W` complete snapshot creations and up to `W * P` process-entry comparisons. `simpleLaunch` can then invoke `FindFirstWindowByProcessName`, causing another snapshot (`cmd/taskswitcher-toolbar/main.go:878-890`). Post-launch title matching repeats the same work 1.5 seconds later (`cmd/taskswitcher-toolbar/main.go:934-949`).

Toolhelp is synchronous and has no deadline in this code. Antivirus, process churn, a heavily loaded kiosk, or an unusual target process can make this much slower than its normal timing. The three-second state reset then allows copies of the lookup to overlap.

Recommendation: take one process snapshot per refresh and build `PID -> executable` and `executable -> []PID` maps, then enumerate windows once. Better still, cache the selected `HWND` per task and validate it with `IsWindow` on each click, using a one-pass rescan only when the cache is invalid. A `SetWinEventHook`-based index can update the cache when top-level windows appear, disappear, or change title; periodic bounded rescanning is a simpler acceptable first version.

### High: selecting the first PID is not selecting the application's window

Evidence: `findFirstPidByName` returns on the first executable-name match (`internal/winutil/winutil_windows.go:66-87`). `FindFirstWindowByProcessName` searches only that PID for a visible top-level window (`internal/winutil/winutil_windows.go:57-63`).

Multi-process applications commonly have several PIDs with the same executable. The first process in a snapshot may be a helper or renderer with no visible top-level window. A valid window owned by a later PID is never considered. The name comparison is also case-sensitive even though Windows executable matching should be treated case-insensitively.

Consequences include false "not running" results, duplicate launches, failure to switch, and repeated user taps. The example configuration explicitly includes `msedge`, for which first-PID selection is especially unsuitable.

Recommendation: enumerate all matching PIDs and all candidate windows. Apply task-specific matching in this order: valid cached handle, executable plus title/class predicate, any eligible window for any matching PID, then launch policy. Normalize names with `strings.EqualFold` and consistently accept names with or without `.exe`.

### High: `switchOnly` is not enforced

Evidence: `Task.IsSwitchOnly` is defined in `internal/config/config.go:42-49`, and `config-example.json` sets it for the browser task, but `simpleLaunch` never calls it. After lookup failure it proceeds to start the configured target (`cmd/taskswitcher-toolbar/main.go:896-925`).

This turns a matching defect or transient slow lookup into a process launch. On a kiosk, this can create multiple browser or control-system instances and consume enough resources to make the UI appear hung.

Recommendation: enforce launch policy before either `exec.Command` or `ShellExecute`. A switch-only miss should return a visible status such as "DSP is not running" and a diagnostic result, never start a process.

### High: synchronous logging can block the UI thread

Evidence: in debug mode the standard logger writes directly to a file (`cmd/taskswitcher-toolbar/main.go:212-238`). `wndProc` and the subclassed button procedure call `log.Printf` before or during command and timer processing (`cmd/taskswitcher-toolbar/main.go:477-595` and `601-637`). A Go ticker logs through the same logger every second (`cmd/taskswitcher-toolbar/main.go:409-437`). The standard logger serializes writers with a mutex.

If a log write blocks, the goroutine performing it holds the logger lock. When the UI thread next logs, it waits for that lock and stops dispatching Windows messages. This is more likely when debug logging is enabled, the disk is unhealthy or busy, the log is scanned by security software, or the working directory is on slow storage. Logging every UI and Go heartbeat adds noise while reducing the diagnostic value of the file.

The separate `debugLog` function calls `Sync` on every line but is unused (`cmd/taskswitcher-toolbar/main.go:163-169`). It should not be adopted as a fix; forcing every record to disk would make the blocking risk larger.

Recommendation: never write logs from the UI thread. Send compact events to a bounded channel and let a dedicated writer batch them. Drop and count low-priority heartbeat messages when the channel is full. Rotate logs by size and retain the last slow-operation and UI-latency events. Default operational logging should be enabled at a low volume rather than discarding all evidence unless `-debug` is supplied.

### Medium: the heartbeat detects a stalled queue but cannot recover it

Evidence: the UI heartbeat is a `WM_TIMER`; its receipt updates `lastTimerAt`. When the Go ticker observes a gap, it calls `SetTimer` again and posts another `WM_TIMER` (`cmd/taskswitcher-toolbar/main.go:393-437`).

A Windows timer message is handled by the same message thread being monitored. If that thread is blocked, posting more messages cannot make it run. While the block continues, the ticker can add another synthetic timer message each second. When processing resumes, queued diagnostics compete with useful input. Calling the mechanism a watchdog gives a false impression that it can restore UI service.

Recommendation: the in-process monitor should only record UI latency. An out-of-process supervisor should decide restart policy. Before terminating a non-responsive process, the supervisor should capture a dump or at least the thread wait chain; otherwise each restart destroys the evidence needed to find the blocking call. Health should distinguish:

- process alive;
- UI thread dispatching messages;
- command worker idle/busy and operation age;
- target application launch/focus result.

### Medium: duplicate click synthesis and de-bouncing are needlessly complex

Evidence: native buttons already notify their parent with `BN_CLICKED`, but each button is subclassed and `WM_LBUTTONUP` posts a second synthetic `BN_CLICKED` (`cmd/taskswitcher-toolbar/main.go:601-632`). A 90 ms map suppresses the expected duplicate. A second 200 ms mechanism can turn a rapid duplicate into a delayed action: when idle it schedules `pendingTask` after 120 ms, then may schedule it again until it falls outside the 200 ms window (`cmd/taskswitcher-toolbar/main.go:791-821`).

Consequences include non-deterministic ordering, delayed duplicate launches, more timers, and a larger callback surface. It also obscures whether a field click problem is caused by Windows input, application state, or the custom subclass.

Recommendation: remove button subclassing and synthetic commands. Handle the native `BN_CLICKED` once. Disable or visually mark only the requested button while the serialized worker handles it. Coalesce identical queued destinations rather than using time-based recursive callbacks.

### Medium: launched process handles are not released

Evidence: the toolbar calls `cmd.Start()` and retains neither a `Wait` nor `Process.Release` path (`cmd/taskswitcher-toolbar/main.go:908-932`). The simple entry point does the same. A long-running kiosk that launches or relaunches targets repeatedly can retain OS process resources until garbage collection/finalization happens.

Recommendation: after a successful detached start, call `cmd.Process.Release()` when the child is intentionally not supervised. If Task Switcher should supervise a child, retain explicit child state and reap it with `Wait` in a dedicated goroutine. Do not mix the two ownership models.

### Medium: shell launch can be slow and is not observable

Evidence: associated files such as `.c3p` are launched with synchronous `ShellExecute` from a worker (`cmd/taskswitcher-toolbar/main.go:953-1000`). It returns only a legacy status value and provides no process handle. Shell association and DDE activation can take an unbounded amount of time from the application's perspective.

Moving it off the UI thread prevents a direct UI block, but the existing three-second watchdog makes a slow shell activation unsafe by allowing another activation to overlap.

Recommendation: use `ShellExecuteEx` with an explicit launch policy and useful flags, capture a process handle when Windows supplies one, time the call, and never overlap activation of the same task. If the `.c3p` association can be resolved reliably at install time, configuring the owning executable and arguments directly is more deterministic than invoking a file association.

### Medium: focus failures are silent

Evidence: `ShowWindow` and `SetForegroundWindow` return Boolean results, but `bringWindowToFront` ignores them (`cmd/taskswitcher-toolbar/main.go:857-864`). Windows may reject foreground activation. The toolbar then reports completion only to a debug log and gives the user no state.

Recommendation: verify the foreground window after the focus attempt and report success, retryable failure, not-running, and launch failure as distinct outcomes. Keep the toolbar responsive and show a short status on the relevant button. A user should not need to tap repeatedly to determine whether the request was accepted.

### Medium: the repository cannot identify a reproducible deployed version

Observed state:

- Source declares `AppVersion = "0.1.4-dev"`.
- `version.txt` contains `0.1.2-dev`.
- The checked-in toolbar executable contains `0.1.4-dev` and was built after `version.txt`.
- The toolbar executable inside `taskswitcher-toolbar.zip` has a different checksum and size from the standalone executable.
- The working-tree modification time of `cmd/taskswitcher-toolbar/main.go` is newer than both packaged artifacts. Modification times alone cannot identify which source produced a binary.
- All tracked history is currently one commit, so field changes cannot be correlated with releases.

It is therefore unclear whether a kiosk runs the archive binary, standalone binary, a build from current source, or another version. A correct source fix is not sufficient if the deployment can silently select an older artifact.

Recommendation: produce binaries in CI or one scripted release command, derive one version from the commit, embed it at link time, create a checksum manifest, and do not track ambiguous mutable binaries at repository root. On startup, log the version, executable path, configuration path, and configuration hash. Expose the same data through a diagnostic command. Keep rollback packages immutable and uniquely named.

### Medium: multiple entry points have diverged

There are four implementations under `cmd`, using Fyne, Walk, direct Win32, and console interaction. `cmd/taskswitcher-ui` no longer builds because its `UI` type lacks fields used throughout its methods. The root Fyne implementation contains an unbounded sleeping goroutine created on each keyboard click. The toolbar has its own Win32 path and does not use the shared keyboard package.

This increases dependency size, makes `go test ./...` unsuitable as a release gate, and leaves maintainers unsure which defects matter in production.

Recommendation: declare `cmd/taskswitcher-toolbar` the one supported application, move prototypes to history or a clearly excluded `experiments` area, and remove Fyne/Walk dependencies if no production command uses them. A release should fail unless every supported package builds and tests pass.

### Low but definite correctness and maintenance issues

- Configuration has two hard-coded task slots (`CloudTask` and `WaveFrontTask`) rather than a task list with stable IDs. `safeButtonClick` uses process name as identity; if two buttons use the same executable with different titles or arguments, the first configuration wins.
- `SystemLabel`, `KeyboardHeight`, and `TextColor` do not meaningfully affect the toolbar. `applyButtonColors` is a placeholder.
- The default config path and debug log path are relative to the process working directory. Startup mechanisms often supply a different directory, making behavior dependent on how the executable was launched.
- JSON permits unknown fields and has no semantic validation for duplicate IDs, missing paths, invalid position/color, conflicting launch modes, or a title requirement for a multi-process target.
- Most Windows API errors are discarded. This prevents useful distinction between "not found", access denied, invalid handle, focus rejected, and API failure.
- There is no single-instance mutex. A supervisor restart race can leave more than one topmost toolbar if the old process has not actually exited.
- There are no unit tests, Windows integration tests, responsiveness thresholds, soak tests, or handle-count checks.

## Recommended architecture

### Retain a small native toolbar

For the stated behavior—always-visible kiosk navigation that activates native application windows—the direct Win32 implementation is a sensible foundation. A browser-based menu would still need a privileged native helper to enumerate and focus windows, creating two components and more failure modes. A large cross-platform GUI framework adds little value for two or three native buttons.

Windows-managed kiosk/shell configuration may still be used to restrict the account and restart the menu, but it does not remove the need for deterministic task matching if the kiosk must switch among these specific applications. Replacing the Windows shell entirely should be considered only if lockdown requirements demand it; it raises recovery and servicing risk.

### Separate the system into four small components

1. **UI thread**: create controls, paint status, accept native click notifications, and post commands. It must not enumerate processes, launch targets, wait on locks held by I/O code, or write to disk.
2. **Command worker**: one goroutine owns an input queue and serially executes switch/launch requests. It publishes immutable results to the UI via `PostMessage`. Repeated requests are coalesced by task ID.
3. **Window index**: one bounded refresh produces candidate windows for all configured tasks. It caches validated handles and avoids process snapshots inside an `EnumWindows` callback.
4. **Diagnostics**: a bounded asynchronous event writer records UI latency, operation timings, decisions, Windows error codes, versions, and resource counts. An external supervisor reads a minimal liveness endpoint or uses a timeout-safe window ping.

The key state transition should be explicit:

`idle -> requested(task ID) -> locating -> focusing | launching -> verifying -> success | failure -> idle`

Each request should carry a monotonically increasing ID. A late result may be logged but must not overwrite state belonging to a newer request.

### Model configuration around intent

Replace the two named fields with a list similar to:

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
      "match": {
        "executable": "CrestronXPanel.exe",
        "titleContains": "Xpanel"
      },
      "launch": {
        "mode": "associatedFile",
        "path": "C:\\embr\\crestron\\GhostDonkeyv1.1.c3p"
      }
    }
  ]
}
```

Launch mode should be one of `switchOnly`, `executable`, or `associatedFile`, not a collection of partly overlapping Booleans. Reject invalid configurations before creating the hidden-console GUI, and write the error to the Windows event log or a guaranteed diagnostic location.

## Implementation plan

### Phase 1: make the current toolbar safe

1. Remove the three-second mutation of `processing`; record a slow-operation event instead.
2. Add one serialized, capacity-one/coalescing task worker with request IDs.
3. Replace title lookup with a single process snapshot plus a single window enumeration; match every PID and use case-insensitive normalized executable names.
4. Enforce `switchOnly` and release or explicitly supervise every process handle.
5. Remove button subclassing, synthetic click messages, and recursive delayed de-bouncing.
6. Move all file logging off the UI thread and reduce heartbeat logging to transitions and threshold violations.
7. Add a named mutex so only one toolbar instance can run.
8. Resolve default configuration and log paths relative to the executable or an explicit application-data directory, not the current directory.

### Phase 2: make behavior observable and testable

1. Introduce interfaces around window enumeration, focus, process launch, clock, and logging.
2. Unit-test multi-PID matching, title matching, case normalization, switch-only misses, coalescing, late completion, and launch failure.
3. Measure each native operation separately rather than only timing the whole button action.
4. Send worker results to the UI and display concise progress/failure state.
5. Have the external watchdog capture a dump before termination and use separate UI-liveness and operation-duration thresholds.
6. Delete or quarantine unsupported entry points and remove unused UI dependencies.

### Phase 3: validate on an actual kiosk image

Run a Windows integration and soak test with the same security software, user policy, Crestron association, browser configuration, and watchdog used in production. Include:

- both targets stopped, starting, running, minimized, hidden, and crashed;
- many processes and top-level windows;
- rapid alternating taps and touch-generated double input;
- target startup longer than the watchdog threshold;
- disk-full and deliberately slow log writing;
- an application with multiple same-name PIDs;
- standby/resume, display resolution changes, and Explorer/shell restarts;
- 72-hour operation while sampling process count, goroutine count, handle count, memory, UI heartbeat latency, and action latency.

Suggested acceptance criteria:

- The toolbar continues dispatching UI messages during every task operation.
- A click is visibly acknowledged within 100 ms.
- Only one switch/launch operation is active at a time.
- A switch-only task is never launched.
- One physical tap causes at most one accepted request.
- Existing target windows are found across all matching PIDs.
- Process handle count and goroutine count remain bounded during the soak test.
- No supervisor termination occurs; if a forced fault is injected, a diagnostic dump and the last operation timeline are retained.
- The installed binary checksum, embedded version, and configuration hash match the release manifest.

## How to prove the field hang before and after the fix

Source review identifies unsafe paths but is not a substitute for capturing the actual wait. On an affected kiosk:

1. Record the exact toolbar binary checksum, command line, working directory, config, watchdog product/configuration, and kill threshold.
2. Enable bounded diagnostic events for UI heartbeat latency and entry/exit timing around process snapshot, window enumeration, `GetWindowText`, `SetForegroundWindow`, process start, and shell activation.
3. Configure the supervisor to capture a full process dump immediately before killing a non-responsive toolbar. Preserve Windows event logs for the same time window.
4. Correlate the last UI heartbeat with the goroutine stacks and native thread wait chains:
   - UI thread waiting on the logger indicates synchronous logging contention.
   - worker threads inside Toolhelp/window discovery support the snapshot-amplification finding.
   - a worker inside shell/DDE activation supports the associated-file launch finding.
   - multiple workers in the same action confirm the three-second overlap defect.
5. Repeat the same workload with the serialized worker and one-pass index. Retain timings rather than relying on the absence of a visible freeze.

Without a dump or toolbar log from a real incident, it would be premature to claim one exact root cause. The overlap defect, ignored switch-only policy, first-PID matching, and inefficient window lookup are definite correctness problems and should be fixed regardless; the logging and shell paths are credible environment-dependent stall sources that instrumentation should confirm or rule out.

## Immediate operational mitigations pending a code fix

- Verify which executable is installed by checksum; do not assume the ZIP and standalone binaries are equivalent.
- Avoid `-debug` as a continuous production setting until logging is decoupled from the UI. Use it only for a controlled reproduction while collecting a dump.
- Ensure the external watchdog captures evidence and allows the old process to exit before starting another instance.
- Do not repeatedly tap a task while it is starting; the current timeout can overlap launches after three seconds.
- For a task that must never be launched by the toolbar, temporarily remove its `processFilePath` as defense in depth. The current `switchOnly` flag alone is ineffective.
