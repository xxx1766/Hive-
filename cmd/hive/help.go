package main

import (
	"fmt"
	"io"
	"os"
	"text/tabwriter"
)

// cmdHelp is one command's user-facing documentation. `brief` shows up in
// the top-level `hive help` table; `long` shows up in `hive help <cmd>`.
type cmdHelp struct {
	usage string
	brief string
	long  string
}

// cmdHelps is the single source of truth for CLI docs. Adding a command
// means registering it here and (for non-trivial behaviour) adding a case
// in dispatch.go. Keeping the registry separate from dispatch.go keeps
// the top-level help from drifting out of sync with actual capabilities.
var cmdHelps = map[string]cmdHelp{
	"version": {
		usage: "hive version",
		brief: "print CLI + daemon version",
	},
	"ping": {
		usage: "hive ping",
		brief: "check the daemon is responsive",
	},
	"build": {
		usage: "hive build <dir>",
		brief: "package an Agent source directory as a Hive Image",
		long: `Reads <dir>/agent.yaml, validates it, and copies the directory into
the local image store (~/.hive/images/<name>/<version>/).

For kind=binary Agents, <dir>/<entry> must already be built (typically
./bin/<name>). For kind=skill / kind=workflow no compilation is needed —
the directory just needs the relevant .md / .json files.`,
	},
	"agents": {
		usage: "hive agents",
		brief: "list locally-installed Agents",
		long: `Lists every Agent that's been built or pulled into the local store
(~/.hive/images/<name>/<version>/). Output format is "<name>:<version>",
one per line.

The on-disk format is still called a "Hive Image" — 'hive agents' is
the user-facing view of "what can I hire right now?".`,
	},
	"pull": {
		usage: "hive pull <url>",
		brief: "fetch a remote Agent into the local store",
		long: `Supported URL forms:
  github://owner/repo/path[@ref]                        scheme form
  https://github.com/owner/repo/tree/ref/path           browser URL
  owner/repo#path[@ref]                                 short (go-get-ish)

@ref defaults to 'main' and can be a tag, branch, or full commit SHA.
For stability, pin to a specific commit SHA in production.

kind=binary Agents are NOT pullable (platform-specific + trust). Use
kind=skill or kind=workflow for distributable Agents.`,
	},
	"init": {
		usage: "hive init <name>",
		brief: "create a new Room",
		long: `Creates ~/.hive/rooms/<name>-<timestamp>/ and registers the Room with
hived. Prints the RoomID; capture it for subsequent hire/run/team/stop.`,
	},
	"rooms": {
		usage: "hive rooms",
		brief: "list all Rooms",
	},
	"hire": {
		usage: "hive hire <room> <ref> [flags]   |   hive hire -f <hivefile-or-url> [--room <name>]",
		brief: "hire one Agent into a Room, or batch-hire from a Hivefile",
		long: `Two shapes:

1. Single hire (Room must already exist):
     hive hire <room> <ref> [--rank <name>] [--model <name>] [--quota <json>] [--volume <n>:<mp>[:<m>]]... [--no-prompt]

   When stdin is a terminal AND no override flags are given, hire prompts
   interactively for rank / model / tokens / http / volumes (Enter to keep
   manifest defaults; Ctrl-D to skip remaining). Pass --no-prompt to disable
   even on a TTY. Piped/scripted invocations never prompt.

   --model overrides the manifest's 'model:' field (sets HIVE_MODEL env).
   Useful when running through a non-OpenAI-direct gateway (e.g. GMI's
   openai/gpt-5.4-mini) without editing the agent's yaml + rebuilding.

   <ref> may be:
     name:version                             local Agent (from the store)
     github://owner/repo/path[@ref]           remote, auto-pulls
     https://github.com/owner/repo/tree/...   browser URL
     owner/repo#path[@ref]                    short form

   Flags:
     --rank <name>             override the Agent's manifest default Rank
                               (intern / staff / manager / director)
     --model <name>            override the Agent's manifest LLM model (sets
                               HIVE_MODEL). Skill agents pick this up directly;
                               workflow agents pick it up only when their
                               flow.json doesn't pin "model" explicitly.
     --quota <json>            override per-resource quota caps. JSON shape:
                                 {"tokens":{"gpt-4o-mini":500},"api_calls":{"http":5}}
                               Partial: keys not in the JSON keep the Rank default.
     --volume <n>:<mp>[:<m>]   bind-mount a named Volume into the Agent's sandbox.
                               <n>=volume name (create with 'hive volume create')
                               <mp>=absolute mountpoint (e.g. /shared/kb)
                               <m>=ro|rw (default ro). Can be repeated.

2. Batch hire from a Hivefile (auto-creates the Room):
     hive hire -f <hivefile-or-url> [--room <name>]

   <hivefile-or-url> accepts a local path or any of the three remote URL
   forms accepted by 'hive pull'. Each agent's 'image:' inside the Hivefile
   may also be remote.

   Stdout receives only the new RoomID, so 'ROOM=$(hive hire -f file.yaml)'
   is safe in shell scripts; per-agent progress goes to stderr.

   Flags:
     --room <name>   override the Room name declared in the Hivefile.
                     Useful for running the same Hivefile as multiple
                     independent Rooms (parallel demos/experiments).

   --rank/--quota/--volume are per-agent and live inside the Hivefile;
   they are rejected on 'hive hire -f'.`,
	},
	"team": {
		usage: "hive team <room>",
		brief: "list Agents in a Room with remaining quota",
	},
	"run": {
		usage: "hive run <room> [--target <image>] [task-json]",
		brief: "dispatch a task to an Agent; streams log output",
		long: `Dispatches [task-json] to the Agent named by --target, or the first
one hired if --target is omitted. Streams the Agent's structured log
events back as the task progresses; prints the final task/done result
to stdout.

[task-json] must be valid JSON; a plain string is automatically wrapped
as a JSON string literal for convenience.`,
	},
	"stop": {
		usage: "hive stop <room>",
		brief: "stop a Room and terminate its Agents",
	},
	"logs": {
		usage: "hive logs <room> [<agent>]",
		brief: "dump persisted Agent stderr logs from a Room",
		long: `Snapshot of the per-Agent stderr files under
~/.hive/rooms/<roomID>/logs/<agent>.stderr.log.

Without <agent>, prints every Agent's log with a header line per Agent.
No tail / follow — use 'tail -f' against the files directly for that.`,
	},
	"volume": {
		usage: "hive volume <create|ls|rm> [args...]",
		brief: "manage named persistent Volumes (cross-Room shared storage)",
		long: `Volumes are named persistent containers under ~/.hive/volumes/<name>/.
Any Room's Agents can read / write a Volume through the memory/* API
(scope = volume name). Use for shared caches, knowledge bases, cross-
Room facts.

Subcommands:
  hive volume create <name>   create a new Volume
  hive volume ls              list all Volumes
  hive volume rm <name>       delete a Volume and everything in it

Name rules: [A-Za-z0-9_-]{1,64}. No spaces, slashes, or dots.

See 'docs/TUTORIAL.md' §跨 Room 共享记忆 for the full story.`,
	},
	"update": {
		usage: "hive update [--check] [--force] [--ref <branch|tag|sha>] [--source-dir <path>] [--prefix <path>]",
		brief: "pull latest hive source, rebuild, and reinstall",
		long: `Updates the locally-installed hive binaries to the latest source.
Equivalent to running 'git pull --ff-only && make build && scripts/install.sh
--skip-build' in the source tree.

Source tree resolution (in order):
  1. --source-dir <path>
  2. ~/.hive/install.json (breadcrumb dropped by scripts/install.sh)
  3. walk up from the running hive binary looking for .git + Makefile

Flags:
  --check               compare local vs upstream, print result, do nothing
  --force               rebuild + reinstall even if already at upstream
  --ref <ref>           git checkout <ref> before pulling (branch/tag/sha)
  --source-dir <path>   override breadcrumb / fallback resolution
  --prefix <path>       install PREFIX (default /usr/local; matches install.sh)

After install, hived may still be running the previous binary in memory —
the command prints a restart hint when it detects a running daemon.`,
	},
	"help": {
		usage: "hive help [<command>]",
		brief: "show this help or per-command help",
		long: `'hive help' alone lists every command with a one-line description.
'hive help <command>' shows detailed usage for that command.

'hive <command> --help' and 'hive <command> -h' behave the same as
'hive help <command>'.`,
	},
}

// cmdOrder controls the top-level help's display order — roughly grouped:
// diagnostics / image lifecycle / room lifecycle / task lifecycle / debug.
var cmdOrder = []string{
	"version", "ping",
	"build", "agents", "pull",
	"volume",
	"init", "rooms",
	"hire", "team", "run", "stop", "logs",
	"update",
	"help",
}

// printTopHelp writes the "no args" / "hive help" output.
func printTopHelp(w io.Writer) {
	fmt.Fprint(w, `hive — Docker for Agents

Usage:
  hive <command> [args...]

Run 'hive help <command>' or 'hive <command> --help' for detailed help.

Commands:
`)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, name := range cmdOrder {
		h := cmdHelps[name]
		fmt.Fprintf(tw, "  %s\t%s\n", name, h.brief)
	}
	tw.Flush()
}

// printCommandHelp writes detailed help for cmd to w and returns true.
// Returns false if cmd isn't registered.
func printCommandHelp(cmd string, w io.Writer) bool {
	h, ok := cmdHelps[cmd]
	if !ok {
		return false
	}
	fmt.Fprintf(w, "%s — %s\n\n", cmd, h.brief)
	fmt.Fprintf(w, "Usage:\n  %s\n", h.usage)
	if h.long != "" {
		fmt.Fprintf(w, "\nDetails:\n%s\n", h.long)
	}
	return true
}

// maybeHandleHelpFlag checks whether the first arg is -h/--help and, if
// so, prints the command's detailed help and returns true (caller should
// return from its handler). Individual cmd functions call this at their
// top to support `hive <cmd> --help`.
func maybeHandleHelpFlag(cmdName string, args []string) bool {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		printCommandHelp(cmdName, os.Stdout)
		return true
	}
	return false
}
