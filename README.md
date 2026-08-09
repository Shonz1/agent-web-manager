# agent-web-manager

A single self-contained Go binary that serves a local web UI for creating
[Docker Sandboxes](https://docs.docker.com/ai/sandboxes/) (`sbx`) and running
terminal sessions inside them. Each sandbox is an isolated container; each
session is a live terminal in the browser attached to something running in it.

```
browser ──WebSocket──▶ manager ──PTY──▶ sbx run|exec ──▶ sandbox container
```

A **sandbox** is the durable thing: created once, persisted, and left running
when the manager exits. A **session** is ephemeral — a PTY attached to the
sandbox's agent or to a shell — and a sandbox can host several at a time.

> This project — code, UI, and documentation, this README included — was
> generated entirely by [Claude](https://claude.com/claude-code).

## Requirements

- Go 1.24+ to build (developed against 1.26)
- Docker and the `sbx` CLI on `PATH` (`sbx version` should work)
- `git` on `PATH`, for the diff a session shows of its workspace; everything
  else works without it

## Build and run

```bash
make build
```

That produces `bin/agent-web-manager` — one binary with the entire UI (HTML,
CSS, xterm.js) embedded via `go:embed`. Nothing else needs to be deployed
alongside it.

```bash
./bin/agent-web-manager
```

Then open <http://127.0.0.1:7788>.

### Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-addr` | `127.0.0.1:7788` | Listen address |
| `-sbx` | `sbx` | Path to the `sbx` binary |
| `-git` | `git` | Path to the `git` binary, used to read a workspace's changes |
| `-state-dir` | `~/Library/Application Support/agent-web-manager` (macOS) | Where sandbox records are persisted |
| `-version` | | Print version and exit |

### Environment

[Telegram notifications](#telegram) are normally set up on the Settings page.
These are the override, for a deployment that would rather not have a
credential written by a web page; when they are set, the page shows the
configuration but will not change it.

| Variable | Meaning |
| --- | --- |
| `AWM_TELEGRAM_TOKEN` | Bot token |
| `AWM_TELEGRAM_CHAT_ID` | Chat to send to |
| `AWM_LINK_BASE` | Address the *Open session* button points at; optional, no button without it |

None of them is a flag, on purpose — see [Telegram](#telegram).

## Using it

### 1. Create a sandbox

**New sandbox** → **Create one**: pick an agent (`claude`, `codex`, `gemini`,
`shell`, …) and a host directory to use as the workspace. **Browse…** opens a
folder picker that walks the host filesystem (git checkouts are tagged), or
type the path directly — `~` is expanded. Optionally set a sandbox name, extra
workspaces (append `:ro` for read-only), and published ports.

This runs `sbx create <agent> --name <name> <workspace>` and nothing else. No
agent starts yet; the first create for an agent can take minutes while its
image is pulled.

**New sandbox** → **Add an existing one** instead lists sandboxes `sbx` already
knows about, including ones created straight from the CLI or left over from a
previous state directory. Its agent and workspaces come from the sandbox
itself. Sandboxes this manager already lists show up marked **already added**
and cannot be picked — adding one twice is not a thing.

### 2. Start sessions in it

Select the sandbox and start as many terminals as you want:

- **Start agent** runs the sandbox's own agent (`sbx run --name <name>`, with
  any arguments you pass after `--`). Each attachment gets its own agent
  process on its own TTY, so several can work in one sandbox at once.
- **Start shell** opens an interactive shell beside them (`sbx exec -it
  <name> bash`) — run a build in one while the agents work in others.

Neither is limited. Anything a session draws — full TUIs included — renders in
the terminal. Sessions appear nested under their sandbox in the sidebar; click
one to attach.

### Session names

Sessions are named for you from the context they start in, so a sandbox with
several of them stays readable:

| Session | Name |
| --- | --- |
| First agent session in a `claude` sandbox | `claude` |
| Second, then third | `claude 2`, `claude 3` |
| Agent session started with `--continue` | `claude --continue` |
| Another with the same arguments | `claude --continue 2` |
| Shell sessions | `shell`, `shell 2`, … |
| Agent session in a `shell` sandbox | `shell agent` |

A number is only appended when one is needed, and a session that ends gives
its number back, so names stay short in a long-lived sandbox. Names are
assigned by the manager, so every browser tab calls a session the same thing.

### What the agent calls itself

Agents record what they are working on — "Create schema-based query service",
not `claude 2` — and a session shows that under its name once there is one.
`sbx` knows nothing about it: it lives in the agent's own state inside the
container, so the manager reads it back with `sbx exec` every 15 seconds while
the session is live.

| Agent | Shown | Read from |
| --- | --- | --- |
| `claude` | the title it generates for the conversation | its transcript, `~/.claude/projects/…` |
| `opencode` | the title it generates for the session | `opencode session list` |
| `codex` | the prompt the session opened with — codex generates no title, and this is what it stores as one | the newest rollout, `~/.codex/sessions/…` |

The two names sit side by side rather than one replacing the other: a session
you are looking for must not rename itself while you look. It appears in the
sidebar, on the sandbox's session cards, and above the terminal, cut to one
line.

Matching a session to its own record is the hard part, and it works two ways:

- **Pinned.** `claude` can be told which conversation to be, so each agent
  session is started under a conversation ID of its own (`--session-id`) and
  read back by it. That holds however many agents share the sandbox.
- **By position.** `codex` and `opencode` cannot, so the newest record for the
  sandbox's workspace is taken as the session's. That is only true while the
  sandbox has a single agent session live — with two, neither takes a new title
  rather than risk showing one the other earned. A record whose working
  directory is not this sandbox's workspace is refused outright.

It is best-effort, and absent in a few cases:

- Agents other than the three above keep their state in formats this manager
  does not know, and keep just their plain name.
- A `claude` session started with `--continue` or `--resume` keeps its plain
  name: claude refuses a pinned conversation ID alongside those unless the
  session is forked, and forking would quietly continue the conversation
  somewhere else. Passing your own `--session-id` works and is followed.
- Nothing appears until the agent has written something, which takes a first
  exchange — an idle session has no record yet. A restarted session starts a
  new conversation, so its old title goes with it.

### What a shell is doing

A shell names nothing, so a shell session shows its last command in the same
place — `shell` on the first line, `go test ./...` under it. Nothing is read
out of the container for this: the manager assembles the line from the
keystrokes it is already forwarding to the PTY, and takes it when Enter is
pressed. Backspace, Ctrl-C, Ctrl-U, Ctrl-W and arrow keys are followed; a
restart clears it.

That makes it an approximation, and it is worth knowing where it drifts. The
manager sees what was typed, not what the shell made of it: a filename Tab
completed was never typed, so it is missing, and keystrokes typed into a
full-screen program started from the prompt are not a command line at all.

### Working, or waiting on you

A live session also shows what it looks like to be doing, as a dot beside its
name: pulsing green while it works, dim green sitting at its prompt, and amber
when it has stopped on a question and cannot go on until someone answers it.
That last one is the point of the whole thing — it is the only state where the
session is waiting for you rather than the other way round, and it is
invisible otherwise. The words are on the session's card and above its
terminal.

No agent reports any of this, so it is read off the session's own screen. The
manager already sees every byte of PTY output, and replays it into a headless
terminal beside the scrollback: a TUI redraws its status line several times a
second by putting the cursor back over it, so the output holds every frame it
has ever drawn and only the screen those frames add up to says which one is
up. Anything still painting is working, whatever it is painting. A session
that has been quiet for more than a second is read: an interrupt hint (`esc to
interrupt`) means an agent part-way through something slow; the keys a menu
offers (`Enter to select`, `↑/↓ to navigate`), a numbered choice with the
cursor on it, or a plain `[y/n]` mean a question; and neither means idle.

Output this manager provoked does not count as the agent doing anything.
Keystrokes echo, and a resize is answered by redrawing the whole screen —
which is what opening a tab does, since a tab sets the PTY to its own size on
attach. Both are ignored for a moment after the fact, so looking at a session
does not make it look busy. A tab whose size the PTY already has is not passed
on at all. Neither window outlasts the silence that counts as idle, so a
session that really is working never looks stopped for having been read.

One agent does not have to be read at all. codex puts `[ ! ] Action Required`
at the front of the terminal title for as long as an approval is up, which is
the only thing any of them states outright rather than draws, so it is taken
at its word ahead of everything else — including the rule that a session still
painting its screen is working, which an overlay that animates underneath
would otherwise win.

The keys are what is looked for rather than the choices themselves, because
the choices need not be in view — claude asks questions whose options carry a
paragraph each, and the one the cursor is on can be well above the foot of the
screen. It is also why `esc to cancel` counts for nothing either way: a
question offers it, and so does a spinner.

Last of all, and only once nothing on the screen is a prompt, comes the
question mark. An agent that ends its turn by asking something in prose —
codex answering "what have you been meaning to build?" and dropping back to
its input box — leaves behind exactly what an agent that ends it by finishing
leaves behind, down to the grey placeholder in the box. Nothing there is a
widget to recognise, so what the agent last said is all there is: an agent
whose final line ends in `?` is taken to be waiting on the answer. Shells are
not read this way; they are in no conversation, and the confirmations they do
put up are recognised above.

It is an inference, and it is wrong in a few places:

- **Typing steadily at an agent hides it working**, which is the other side of
  not counting the echo: while the keystrokes keep coming, only the screen says
  anything, so an agent whose spinner is not recognised reads as idle until you
  stop. It is the session you are looking at, so there is not much in it.
- **A question this manager does not recognise reads as idle.** Agents draw
  permission prompts as a numbered list, and the ones above are what is looked
  for; something shaped differently is silence like any other. An agent whose
  spinner is unrecognisable still reads as working while it draws it, which is
  the part that matters most.
- **The question mark cuts both ways.** "Let me know which you prefer" is a
  question and is missed; a summary that ends on a rhetorical one is not and is
  caught. It also only counts as the last thing said — six lines further up,
  under whatever the agent did next, an answered question stops counting.
- **A shell is judged the same way**, so a build streaming output reads as
  working and `tail -f` reads as working forever. Both are arguably true.
- Transitions lag by up to a second and a half, which is the price of not
  mistaking a pause for an ending.

### Being told, rather than looking

A dot in a sidebar only works while you are looking at the sidebar. The two
moments worth walking back to the machine for — an agent that has stopped on a
question, and one that has finished a stretch of work — can also be pushed at
you, as a desktop notification or a Telegram message. Both are off until you
turn them on, and both are driven by the same events from the same place, so
they say the same thing at the same time.

**Activity is the wrong signal to notify on directly**, which is most of the
work here. It is sampled four times a second off a screen an agent repaints
while it thinks, so one turn flickers between working and idle several times
before it really ends, and every flicker would be a notification. Each state
therefore has to hold before it counts:

- **A question** has to stay up for three seconds. It is the better-founded of
  the two — matched against widgets the agent draws rather than inferred from
  silence — so this only has to ride out a menu being redrawn.
- **An ending** has to stay quiet for ten. Silence is the weak signal: an agent
  part-way through a long tool call draws nothing either, and only the spinners
  this manager recognises tell that apart from a finished turn.
- **And the work has to have lasted twenty seconds** to count as work at all.
  Every session starts out marked busy and a shell reaches its prompt in under
  a second, so without this, opening one would announce that it had completed a
  task, and so would every `ls`. It is also why answering a question and
  watching the agent tidy up for three seconds tells you nothing: you were
  already there.

Work resuming, a question going away, or the session exiting all withdraw
whatever was pending.

One more rule keeps a single question to a single notification. A question that
is up is not a still picture — the agent repaints the box it is drawn in, and a
repaint is output like any other, so activity leaves `waiting` and comes back a
second later. codex sidesteps this by putting `action required` in the terminal
title, which is believed ahead of the screen and never wavers; claude has no
such title and oscillates, which is why it was the one that announced itself
over and over. So a second question is only announced once **five seconds of
work** have happened since the first, or the session has gone idle — either of
which means the last one is behind you. The cost is that an approval granted
and a new one asked for inside five seconds arrives as one notification rather
than two.

What survives all of that is one notification per thing that actually happened.

Both are set up on the **Settings** page, behind the button at the foot of the
sidebar or at `/settings`. That button wears a bell when either channel is on,
because notifications that are silently not arriving look exactly like a quiet
afternoon.

#### In the browser

**Turn on** under *In this browser* asks the browser for permission and
remembers the answer. Notifications are tagged per session, so a later one
about the same agent replaces the earlier one instead of stacking; clicking one
focuses the tab and opens that session. Nothing is sent for the session you are
already looking at in a visible tab, which is the one case where the screen has
said it already.

The catch is that browsers only hand the notification API to a secure context.
That is satisfied on `localhost` and by HTTPS, and it is *not* satisfied over
plain HTTP on a LAN address — which is exactly the phone case, and so the case
where this would have been most useful. The button says so rather than failing
mutely. Telegram is the answer there, and it is the better answer anyway: it
arrives with the tab closed and the laptop shut.

#### Telegram

Create a bot with [@BotFather](https://t.me/botfather), send it a message, and
read your chat id back out of `getUpdates`:

```bash
curl -s "https://api.telegram.org/bot<token>/getUpdates" | grep -o '"chat":{"id":[0-9-]*'
```

Put both into **Settings → Telegram** and press *Save and connect*. Saving
checks the token and then uses it, because only actually posting proves the bot
can reach the chat — the half people get wrong — and the message that arrives
is the confirmation you wanted anyway. Nothing is written to disk until that
works, so a saved configuration is always one that was working when it was
saved. It takes effect immediately: the relay picks up the new bot without a
restart.

The token is stored in `<state-dir>/telegram.json`, mode 600, and is **never
sent back to the page** — not in the settings, not in an error. It therefore
cannot be pre-filled in the form either, so leaving the token box empty on a
bot that is already set up means "keep the one you have" — that way correcting
the chat or the link does not mean going to find the credential again.

**Link back to this manager at** puts an *Open session* button under each
notification, going straight to the session it is about. It has to be an
address the device *reading* the notification can reach: a phone tapping
`127.0.0.1` reaches itself, so the default loopback bind is no use here. Use
the LAN address the manager is on, or a tailnet or tunnel hostname. The field
suggests whatever address you have this page open at, which is exactly right
when you set it up from the phone. Leave it empty for notifications with no
button.

The address is checked when you save, and the confirmation message is sent
*with* its button — Telegram refuses a whole message over a URL it will not
accept, so an address it dislikes has to fail while you are looking at the form
rather than by the notifications quietly stopping.

*Send a test* posts a message with the configuration as it stands, which is the
only way to find out that a bot someone has since blocked has stopped working.
*Turn off* deletes the file.

There is deliberately no flag for either value: a flag is in the process's
command line, where every other process on the machine can read it out of `ps`,
and a bot token is a bearer credential for the whole bot.

For a deployment that would rather not have a credential written by a web page,
`AWM_TELEGRAM_TOKEN` and `AWM_TELEGRAM_CHAT_ID` still work and take precedence.
The settings page then shows the configuration as read-only and says where it
came from, rather than offering to change something the environment would go on
shadowing. Setting one of the two and not the other refuses to start: it is
always a mistake, and the alternative is notifications that silently never
arrive.

Sends that fail are logged and dropped rather than retried. These describe a
state that is still moving: an event held in a queue for a minute is about a
session that has moved on, and there will be another along shortly.

### What it has actually done

A session has a second view behind the **Changes** tab: the git diff of the
sandbox's workspace, beside the terminal the agent is talking in. What an agent
says it did and what it wrote to the files are not the same claim, and this is
the one that can be checked.

The tab counts the changed files, so a glance at it says whether there is
anything to review without opening it. Picking a file shows its diff, with the
line numbers held against the left edge as long lines scroll under them.
Copying out of the diff gives the source: the `+` and `−` are drawn rather than
written, and the line numbers are not selectable.

Two things are being compared, and the selector picks which:

- **Uncommitted** — everything not committed yet, staged or not, including
  files the agent has created and git has never heard of. This is the usual
  case: work in progress.
- **Whole branch** — everything this branch has changed since it left the
  default branch, commits included. This is what to use once the agent has
  started committing, because uncommitted work alone then shows almost nothing.
  On the default branch itself there is no such stretch, and it falls back to
  uncommitted work and says so.

The workspace is bind-mounted from the host, so the files the agent is editing
inside the sandbox are the ones on the host, and the manager reads them there
with the `git` on your `PATH` (`-git` points it elsewhere). That means a diff
is still readable after the sandbox has been stopped, costs nothing while the
tab is closed, and does not care whether the agent's image has git in it. The
list re-reads itself every five seconds while you are looking at it, and leaves
the pane alone when nothing has changed, so reading a long diff is not
interrupted by the agent saving a file.

Nothing here writes. Every command is a read, and `GIT_OPTIONAL_LOCKS=0` keeps
even the ones that would ordinarily refresh the index from taking its lock —
looking at a diff cannot disturb an agent that is halfway through a commit in
the same repository.

A workspace that is not a git checkout, or a sandbox mounted without one, says
so rather than reporting an error.

### 3. Lifecycle

- **Interrupt** sends Ctrl-C to the session.
- **Restart** runs an exited session again, in the same sandbox.
- **End session** kills that terminal and leaves everything else alone.
- **Stop** (sandbox) ends every session in it and stops the container, keeping
  its state.
- **Delete** (sandbox) destroys the container permanently.

Sandboxes are persisted, so restarting the manager keeps them listed. Sessions
are not: a session is a live process with a PTY behind it, and there is nothing
left of one after the manager exits — start them again in the sandbox that came
back. Multiple browser tabs can attach to the same session and see the same
output. Each session keeps 256 KB of scrollback so a tab that attaches late
still sees the current screen.

Whatever is open is in the address bar — `/sandboxes/{id}`, `/sessions/{id}`,
`/settings`, bare `/` for nothing — so reloading the page comes back to it instead of an
empty pane, Back and Forward walk what was opened, and a view can be sent as a
link. A route that no longer resolves falls back to the empty pane: sandbox ids
survive a restart of the manager, session ids do not.

Starting a session in a sandbox `sbx` no longer has recreates it first. A
sandbox that was *added* rather than created here is the exception: the manager
only read its agent and workspaces back, and rebuilding from that would quietly
produce a different sandbox, so it says so instead.

### On a phone

Below 720px the sandbox list becomes a drawer behind the **☰** button in the
top bar, so the terminal keeps the whole screen; picking a sandbox or session
closes it, as does tapping outside. Sessions add a row of keys under the
terminal — `esc`, `tab`, `^C`, and the arrows — because a soft keyboard has
none of them and every agent TUI wants all of them. Dragging on the terminal
scrolls it even while the program inside has mouse reporting on.

Reaching the manager from a phone means binding it somewhere the phone can
see, which puts it on the network — read [Security](#security) first. It also
means plain HTTP on a LAN address, where the browser will not give the page
notifications at all; [Telegram](#telegram) is what reaches a phone, and it
reaches it with the tab closed.

## API

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/api/health` | Whether `sbx` is usable |
| `GET` | `/api/agents` | Agents `sbx` can launch |
| `GET` | `/api/sbx/sandboxes` | `sbx ls` output, each entry flagged `managed` |
| `GET` | `/api/fs/dirs` | Sub-directories of `?path=` for the folder picker |
| `GET` | `/api/events` | Server-sent stream of `attention` and `done` events |
| `GET` | `/api/settings/telegram` | Notification settings, never including the token |
| `PUT` | `/api/settings/telegram` | Check a bot's credentials, then save and use them; an empty `token` keeps the stored one |
| `DELETE` | `/api/settings/telegram` | Turn Telegram off and forget the token |
| `POST` | `/api/settings/telegram/test` | Send a test message |
| `GET` | `/api/sandboxes` | Managed sandboxes, with live status and sessions |
| `POST` | `/api/sandboxes` | Create a sandbox |
| `POST` | `/api/sandboxes/adopt` | Take over a sandbox that already exists |
| `GET` | `/api/sandboxes/{id}` | One sandbox |
| `POST` | `/api/sandboxes/{id}/stop` | End its sessions and stop it |
| `DELETE` | `/api/sandboxes/{id}` | Destroy it permanently |
| `POST` | `/api/sandboxes/{id}/sessions` | Start a session inside it |
| `GET` | `/api/sandboxes/{id}/diff` | Changed files in its workspace; `?base=head` (default) or `branch` |
| `GET` | `/api/sandboxes/{id}/diff/file` | One file's diff, as hunks of numbered lines; `?path=` and, for a rename, `&old=` |
| `GET` | `/api/sessions/{sid}` | One session |
| `POST` | `/api/sessions/{sid}/interrupt` | Send Ctrl-C |
| `POST` | `/api/sessions/{sid}/restart` | Run it again |
| `DELETE` | `/api/sessions/{sid}` | End it |
| `GET` | `/api/sessions/{sid}/attach` | WebSocket terminal |

Create a sandbox:

```bash
curl -X POST localhost:7788/api/sandboxes \
  -H 'Content-Type: application/json' \
  -d '{"agent":"claude","workspace":"/path/to/project","name":"claude-proj"}'
```

Start an agent session in it (`{id}` is the sandbox ID the call above
returned). The response carries the `title` the manager assigned it — and,
once the agent has generated one, later reads of the session carry its own
`aiTitle` beside it. A live session also carries `activity`, one of `busy`,
`waiting` or `idle`; a session that is not running carries none. Repeat the
call for a second agent:

```bash
curl -X POST localhost:7788/api/sandboxes/{id}/sessions \
  -H 'Content-Type: application/json' \
  -d '{"kind":"agent","agentArgs":["--continue"]}'
```

Open a shell alongside them. Reads of a shell session carry its
`lastCommand`, which is where `aiTitle` would be for an agent:

```bash
curl -X POST localhost:7788/api/sandboxes/{id}/sessions \
  -H 'Content-Type: application/json' \
  -d '{"kind":"shell"}'
```

Review what an agent has done to the workspace without attaching to it. The
first call lists the changed files with their line counts; the second returns
one file's diff already parsed, so nothing reading this has to understand git's
output:

```bash
curl 'localhost:7788/api/sandboxes/{id}/diff?base=branch'
curl 'localhost:7788/api/sandboxes/{id}/diff/file?path=internal/server/handler.go'
```

Both check the request origin, since between them they return the contents of
your source.

The attach socket carries raw PTY bytes as binary frames and JSON status as
text frames. The browser sends `{"type":"input","data":"…"}` and
`{"type":"resize","cols":N,"rows":N}`.

Watch what every session in every sandbox is doing, without attaching to any
of them:

```bash
curl -N localhost:7788/api/events
```

```
event: attention
data: {"kind":"attention","sessionId":"f705…","sandboxId":"738e…","sandboxName":"my-app","title":"claude","detail":"Add a rate limiter to the API","at":"…"}
```

The dwell that decides which moments appear here is applied before the stream,
not after, so anything reading it — the UI, a bot, a shell script — sees the
same events at the same moment.

## Security

The manager can start agents with full access to any host directory you point
it at, so it binds to `127.0.0.1` by default and rejects cross-origin WebSocket
upgrades and event streams. Do not expose it on a public interface — there is
no authentication.

Configuring a Telegram bot is the one thing that makes this tool talk to the
outside world; until you do, it makes no outbound connections at all. What
leaves the machine is a session's title, whatever the agent called its own
conversation or the last command a shell ran, the sandbox name, and — if you
set one — this manager's address and the session's id, in the button under the
message. That is enough to identify the work by name, which is the point of it.
Worth remembering that a Telegram chat is not a private place: everyone in it
can read the notifications and tap the button.

The bot token is kept out of everything that could carry it away. It is never
returned to the page, and it is redacted from errors before they are logged or
shown: Telegram authenticates by putting the token in the request path, and Go
puts the request URL into every transport error it returns. The settings
endpoints that write it also check the request origin, so a page on another
site cannot point your notifications at a chat of its own.

The folder picker and the workspace diff check it too. Between them they return
directory listings and the contents of your source, which is not something
another site should be able to ask this manager for. Paths reaching the diff
are required to be relative and to stay inside the repository: one of the
commands they end up at is `git diff --no-index`, which would otherwise read
any file on the disk it was pointed at.

That origin check is *not* on the rest of the API. Anyone who can reach this
manager can already start an agent with full access to your source, so the
loopback bind is what is protecting you, not the endpoints — which is the same
reason not to expose it on a public interface.

## Layout

```
main.go                     flags, lifecycle, graceful shutdown
internal/sbx/               sbx CLI wrapper and argv construction
internal/git/               reads a workspace's changed files and their diffs
internal/manager/           sandbox registry and persistence, session PTY lifecycle, scrollback, what a session is doing, which moments are worth notifying about
internal/notify/            Telegram bot, its settings and persistence, and the relay
internal/web/               HTTP API, WebSocket attach, event stream, embedded UI
internal/web/static/        the UI (embedded into the binary)
```

State lives in `<state-dir>/sandboxes.json`, alongside a `telegram.json`
written by the Settings page and readable only by its owner. An install from before sandboxes
and sessions were separate is migrated from `sessions.json` on first start:
each of those sessions was really a sandbox, so it comes back as one.
