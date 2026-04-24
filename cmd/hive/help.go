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
	"images": {
		usage: "hive images",
		brief: "list local Hive Images",
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
	"up": {
		usage: "hive up <hivefile-or-url> [--room <name>]",
		brief: "init a Room + hire all Agents declared in a Hivefile.yaml",
		long: `<hivefile-or-url> accepts either a local path to a Hivefile.yaml or any
of the three remote URL forms accepted by 'hive pull'.

Inside the Hivefile, each agent's 'image:' field may also be a remote
URL — the daemon pulls each one on the fly.

Quota overrides in the Hivefile ('quota:' under each agent) propagate
down to the daemon as partial overrides on top of Rank defaults.

Flags:
  --room <name>  override the Room name declared in the Hivefile.
                 Useful for running the same Hivefile as multiple
                 independent Rooms (e.g. parallel demos/experiments).`,
	},
	"hire": {
		usage: "hive hire <room> <ref> [--rank <name>] [--quota <json>] [--volume <name>:<mountpoint>[:<ro|rw>]]...",
		brief: "hire an Agent into a Room",
		long: `<ref> may be:
  name:version                             local Image (from the store)
  github://owner/repo/path[@ref]           remote, auto-pulls
  https://github.com/owner/repo/tree/...   browser URL
  owner/repo#path[@ref]                    short form

Flags:
  --rank <name>             override the Image's manifest default Rank
                            (intern / staff / manager / director)
  --quota <json>            override per-resource quota caps. JSON shape:
                              {"tokens":{"gpt-4o-mini":500},"api_calls":{"http":5}}
                            Partial: keys not in the JSON keep the Rank default.
  --volume <n>:<mp>[:<m>]   bind-mount a named Volume into the Agent's sandbox.
                            <n>=volume name (create with 'hive volume create')
                            <mp>=absolute mountpoint (e.g. /shared/kb)
                            <m>=ro|rw (default ro). Can be repeated.`,
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
	"build", "images", "pull",
	"volume",
	"init", "rooms", "up",
	"hire", "team", "run", "stop", "logs",
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
