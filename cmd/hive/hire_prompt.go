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

// promptHireOverrides walks the user through rank / model / quota /
// volumes for a single-agent hire. Auto-triggered when stdin is a TTY
// and no override flags were given on the command line. Pass --no-prompt
// to disable even when stdin is interactive.
//
// All five prompts are optional — blank input keeps the manifest / Rank
// default for that dimension. EOF (Ctrl-D) collapses any remaining prompts
// to "blank" so the user can bail at any point and accept defaults.
//
// If the user fills in `model?`, the subsequent `tokens?` prompt is
// simplified to a plain integer (the model is already known); the quota
// key is auto-populated to that model name. If `model?` is left blank,
// `tokens?` falls back to the explicit `<model>:<int>` form so a user
// can still cap a non-default model's budget independently.
func promptHireOverrides(in io.Reader, out io.Writer, refDisplay string) (
	rank, model string,
	quotaRaw json.RawMessage,
	volumes []ipc.VolumeMountRef,
	err error,
) {
	rdr := bufio.NewReader(in)
	fmt.Fprintf(out, "\nConfiguring %s (Enter to keep manifest defaults; Ctrl-D to skip remaining)\n", refDisplay)

	// 1. Rank override
	rank, err = promptLine(rdr, out, "  rank?    [intern/staff/manager/director, blank=keep] > ")
	if err != nil {
		return "", "", nil, nil, err
	}
	if rank != "" {
		switch rank {
		case "intern", "staff", "manager", "director":
		default:
			return "", "", nil, nil, fmt.Errorf("rank: must be intern/staff/manager/director, got %q", rank)
		}
	}

	// 2. Model override (HIVE_MODEL). Affects which LLM the Agent calls
	// (skill-runner reads the env directly; workflow-runner falls back
	// to it when flow.json doesn't pin a model).
	model, err = promptLine(rdr, out, "  model?   [override LLM model, e.g. openai/gpt-5.4-mini, blank=keep] > ")
	if err != nil {
		return "", "", nil, nil, err
	}

	// 3 & 4. Quota — tokens (per-model) + http calls.
	var qo ipc.QuotaOverride
	var tokenLabel string
	if model != "" {
		tokenLabel = fmt.Sprintf("  tokens?  [budget for %s as int, blank=skip] > ", model)
	} else {
		tokenLabel = "  tokens?  [<model>:<int>, e.g. gpt-4o-mini:5000, blank=skip] > "
	}
	tokenLine, err := promptLine(rdr, out, tokenLabel)
	if err != nil {
		return "", "", nil, nil, err
	}
	if tokenLine != "" {
		var key string
		var n int
		if model != "" {
			// User already gave a model — interpret line as a plain int.
			v, perr := strconv.Atoi(tokenLine)
			if perr != nil {
				return "", "", nil, nil, fmt.Errorf("tokens: budget must be int, got %q", tokenLine)
			}
			key, n = model, v
		} else {
			parts := strings.SplitN(tokenLine, ":", 2)
			if len(parts) != 2 {
				return "", "", nil, nil, fmt.Errorf("tokens: want <model>:<int>, got %q", tokenLine)
			}
			v, perr := strconv.Atoi(strings.TrimSpace(parts[1]))
			if perr != nil {
				return "", "", nil, nil, fmt.Errorf("tokens: budget must be int, got %q", strings.TrimSpace(parts[1]))
			}
			key, n = strings.TrimSpace(parts[0]), v
		}
		qo.Tokens = map[string]int{key: n}
	}

	httpLine, err := promptLine(rdr, out, "  http?    [HTTP call budget int, blank=skip] > ")
	if err != nil {
		return "", "", nil, nil, err
	}
	if httpLine != "" {
		n, perr := strconv.Atoi(httpLine)
		if perr != nil {
			return "", "", nil, nil, fmt.Errorf("http: budget must be int, got %q", httpLine)
		}
		qo.APICalls = map[string]int{"http": n}
	}

	if len(qo.Tokens) > 0 || len(qo.APICalls) > 0 {
		b, merr := json.Marshal(qo)
		if merr != nil {
			return "", "", nil, nil, merr
		}
		quotaRaw = b
	}

	// 5. Volume mounts (repeating; blank line ends).
	fmt.Fprintln(out, "  volumes? [<name>:<mountpoint>[:<ro|rw>], blank line=done]")
	for {
		s, lerr := promptLine(rdr, out, "           > ")
		if lerr != nil {
			return "", "", nil, nil, lerr
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

	return rank, model, quotaRaw, volumes, nil
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
