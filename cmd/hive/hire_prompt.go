package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/anne-x/hive/internal/ipc"
)

// promptHireOverrides walks the user through rank / quota / volumes for a
// single-agent hire. Auto-triggered when stdin is a TTY and no override
// flags were given on the command line. Pass --no-prompt to disable even
// when stdin is interactive.
//
// All four prompts are optional — blank input keeps the manifest / Rank
// default for that dimension. EOF (Ctrl-D) collapses any remaining prompts
// to "blank" so the user can bail at any point and accept defaults.
func promptHireOverrides(in io.Reader, out io.Writer, refDisplay string) (
	rank string,
	quotaRaw json.RawMessage,
	volumes []ipc.VolumeMountRef,
	err error,
) {
	rdr := bufio.NewReader(in)
	fmt.Fprintf(out, "\nConfiguring %s (Enter to keep manifest defaults; Ctrl-D to skip remaining)\n", refDisplay)

	// 1. Rank override
	rank, err = promptLine(rdr, out, "  rank?    [intern/staff/manager/director, blank=keep] > ")
	if err != nil {
		return "", nil, nil, err
	}
	if rank != "" {
		switch rank {
		case "intern", "staff", "manager", "director":
		default:
			return "", nil, nil, fmt.Errorf("rank: must be intern/staff/manager/director, got %q", rank)
		}
	}

	// 2 & 3. Quota — tokens (per-model) + http calls.
	var qo ipc.QuotaOverride
	tokenLine, err := promptLine(rdr, out, "  tokens?  [<model>:<int>, e.g. gpt-4o-mini:5000, blank=skip] > ")
	if err != nil {
		return "", nil, nil, err
	}
	if tokenLine != "" {
		parts := strings.SplitN(tokenLine, ":", 2)
		if len(parts) != 2 {
			return "", nil, nil, fmt.Errorf("tokens: want <model>:<int>, got %q", tokenLine)
		}
		n, perr := strconv.Atoi(strings.TrimSpace(parts[1]))
		if perr != nil {
			return "", nil, nil, fmt.Errorf("tokens: budget must be int, got %q", strings.TrimSpace(parts[1]))
		}
		qo.Tokens = map[string]int{strings.TrimSpace(parts[0]): n}
	}

	httpLine, err := promptLine(rdr, out, "  http?    [HTTP call budget int, blank=skip] > ")
	if err != nil {
		return "", nil, nil, err
	}
	if httpLine != "" {
		n, perr := strconv.Atoi(httpLine)
		if perr != nil {
			return "", nil, nil, fmt.Errorf("http: budget must be int, got %q", httpLine)
		}
		qo.APICalls = map[string]int{"http": n}
	}

	if len(qo.Tokens) > 0 || len(qo.APICalls) > 0 {
		b, merr := json.Marshal(qo)
		if merr != nil {
			return "", nil, nil, merr
		}
		quotaRaw = b
	}

	// 4. Volume mounts (repeating; blank line ends).
	fmt.Fprintln(out, "  volumes? [<name>:<mountpoint>[:<ro|rw>], blank line=done]")
	for {
		s, lerr := promptLine(rdr, out, "           > ")
		if lerr != nil {
			return "", nil, nil, lerr
		}
		if s == "" {
			break
		}
		v, verr := parseVolumeFlag(s)
		if verr != nil {
			fmt.Fprintf(out, "    %v (try again, or Enter to finish)\n", verr)
			continue
		}
		volumes = append(volumes, v)
	}

	return rank, quotaRaw, volumes, nil
}

// promptLine writes label and reads one trimmed line from rdr. EOF is
// treated as "blank line" so Ctrl-D mid-prompt accepts defaults for the
// rest. Returns ("", err) only for genuine I/O failures.
func promptLine(rdr *bufio.Reader, out io.Writer, label string) (string, error) {
	fmt.Fprint(out, label)
	line, err := rdr.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// stdinIsTTY reports whether stdin is connected to a terminal. We auto-
// enable the hire prompt only on a TTY so piped scripts (CI, demos,
// `hive hire ... < /dev/null`) never block on user input.
func stdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}
