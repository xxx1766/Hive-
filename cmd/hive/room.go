package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/anne-x/hive/internal/hivefile"
	"github.com/anne-x/hive/internal/image"
	"github.com/anne-x/hive/internal/ipc"
)

func cmdInit(ctx context.Context, args []string) {
	if maybeHandleHelpFlag("init", args) {
		return
	}
	if len(args) < 1 {
		printCommandHelp("init", os.Stderr)
		os.Exit(2)
	}
	c := mustDial(ctx)
	defer c.Close()
	raw, err := c.Call(ctx, ipc.MethodRoomInit, ipc.RoomInitParams{Name: args[0]})
	if err != nil {
		fmt.Fprintf(os.Stderr, "init: %v\n", err)
		os.Exit(1)
	}
	var r ipc.RoomInitResult
	_ = json.Unmarshal(raw, &r)
	fmt.Printf("%s\n", r.RoomID)
}

func cmdRooms(ctx context.Context, args []string) {
	if maybeHandleHelpFlag("rooms", args) {
		return
	}
	c := mustDial(ctx)
	defer c.Close()
	raw, err := c.Call(ctx, ipc.MethodRoomList, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rooms: %v\n", err)
		os.Exit(1)
	}
	var r ipc.RoomListResult
	_ = json.Unmarshal(raw, &r)
	if len(r.Rooms) == 0 {
		fmt.Println("(no rooms)")
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ROOM_ID\tNAME\tSTATE")
	for _, rm := range r.Rooms {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", rm.RoomID, rm.Name, rm.State)
	}
	tw.Flush()
}

func cmdStop(ctx context.Context, args []string) {
	if maybeHandleHelpFlag("stop", args) {
		return
	}
	if len(args) < 1 {
		printCommandHelp("stop", os.Stderr)
		os.Exit(2)
	}
	c := mustDial(ctx)
	defer c.Close()
	_, err := c.Call(ctx, ipc.MethodRoomStop, ipc.RoomStopParams{RoomID: args[0]})
	if err != nil {
		fmt.Fprintf(os.Stderr, "stop: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("stopped")
}

func cmdTeam(ctx context.Context, args []string) {
	if maybeHandleHelpFlag("team", args) {
		return
	}
	if len(args) < 1 {
		printCommandHelp("team", os.Stderr)
		os.Exit(2)
	}
	c := mustDial(ctx)
	defer c.Close()
	raw, err := c.Call(ctx, ipc.MethodRoomTeam, ipc.RoomTeamParams{RoomID: args[0]})
	if err != nil {
		fmt.Fprintf(os.Stderr, "team: %v\n", err)
		os.Exit(1)
	}
	var r ipc.RoomTeamResult
	_ = json.Unmarshal(raw, &r)
	if len(r.Members) == 0 {
		fmt.Println("(no agents)")
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tRANK\tSTATE\tQUOTA REMAINING")
	for _, m := range r.Members {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", m.ImageName, m.Rank, m.State, formatQuota(m.Quota))
	}
	tw.Flush()
}

// parseVolumeFlag accepts "<name>:<mountpoint>" or
// "<name>:<mountpoint>:<ro|rw>". Mountpoint must be absolute.
func parseVolumeFlag(s string) (ipc.VolumeMountRef, error) {
	parts := strings.SplitN(s, ":", 3)
	if len(parts) < 2 {
		return ipc.VolumeMountRef{}, fmt.Errorf("--volume expects <name>:<mountpoint>[:<ro|rw>], got %q", s)
	}
	mode := "ro"
	if len(parts) == 3 {
		if parts[2] != "ro" && parts[2] != "rw" {
			return ipc.VolumeMountRef{}, fmt.Errorf("--volume mode must be ro|rw, got %q", parts[2])
		}
		mode = parts[2]
	}
	if parts[0] == "" {
		return ipc.VolumeMountRef{}, fmt.Errorf("--volume: name cannot be empty")
	}
	if len(parts[1]) == 0 || parts[1][0] != '/' {
		return ipc.VolumeMountRef{}, fmt.Errorf("--volume: mountpoint must be absolute, got %q", parts[1])
	}
	return ipc.VolumeMountRef{Name: parts[0], Mode: mode, Mountpoint: parts[1]}, nil
}

func formatQuota(q map[string]any) string {
	if len(q) == 0 {
		return "(unlimited)"
	}
	parts := make([]string, 0, len(q))
	for k, v := range q {
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
	}
	return strings.Join(parts, " ")
}

// hireOneAgent runs the per-Agent pipeline used by both `hive hire` modes:
// pull-if-remote → ParseRef → daemon AgentHire RPC. Returns the resolved
// local ref (name:version) so callers can render "remote (= local)" when
// the input was a URL.
func hireOneAgent(
	ctx context.Context,
	c *ipc.Client,
	roomID, refInput, rank string,
	quotaRaw json.RawMessage,
	volumes []ipc.VolumeMountRef,
) (ipc.AgentHireResult, string, error) {
	localRef, err := pullIfRemote(ctx, c, refInput)
	if err != nil {
		return ipc.AgentHireResult{}, "", err
	}
	ref, err := image.ParseRef(localRef)
	if err != nil {
		return ipc.AgentHireResult{}, "", err
	}
	raw, err := c.Call(ctx, ipc.MethodAgentHire, ipc.AgentHireParams{
		RoomID:     roomID,
		Image:      ipc.ImageRef{Name: ref.Name, Version: ref.Version},
		RankName:   rank,
		QuotaOverr: quotaRaw,
		Volumes:    volumes,
	})
	if err != nil {
		return ipc.AgentHireResult{}, localRef, err
	}
	var r ipc.AgentHireResult
	_ = json.Unmarshal(raw, &r)
	return r, localRef, nil
}

// cmdHire dispatches between two CLI shapes:
//
//	hive hire <room> <ref> [--rank ...] [--quota ...] [--volume ...]   single-agent
//	hive hire -f <hivefile-or-url> [--room <name>]                     declarative batch
//
// In batch mode, hire creates the Room itself (taking the name from
// hivefile.room, or --room override), then iterates hf.Agents. Stdout
// gets the RoomID so shell scripts can capture it; per-Agent progress
// goes to stderr.
func cmdHire(ctx context.Context, args []string) {
	if maybeHandleHelpFlag("hire", args) {
		return
	}

	// Detect -f / --file anywhere in args; treats both as the same flag.
	for _, a := range args {
		if a == "-f" || a == "--file" {
			cmdHireFile(ctx, args)
			return
		}
	}

	if len(args) < 2 {
		printCommandHelp("hire", os.Stderr)
		os.Exit(2)
	}
	roomID := args[0]
	refInput := args[1]

	rank := ""
	var quotaRaw json.RawMessage
	var volumes []ipc.VolumeMountRef
	noPrompt := false
	for i := 2; i < len(args); i++ {
		switch args[i] {
		case "--rank":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "hire: --rank requires a value")
				os.Exit(2)
			}
			rank = args[i+1]
			i++
		case "--quota":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "hire: --quota requires a JSON object")
				os.Exit(2)
			}
			if !json.Valid([]byte(args[i+1])) {
				fmt.Fprintf(os.Stderr, "hire: --quota value is not valid JSON: %s\n", args[i+1])
				os.Exit(2)
			}
			quotaRaw = json.RawMessage(args[i+1])
			i++
		case "--volume":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "hire: --volume requires <name>:<mountpoint>[:<ro|rw>]")
				os.Exit(2)
			}
			v, err := parseVolumeFlag(args[i+1])
			if err != nil {
				fmt.Fprintf(os.Stderr, "hire: %v\n", err)
				os.Exit(2)
			}
			volumes = append(volumes, v)
			i++
		case "--no-prompt", "--non-interactive":
			noPrompt = true
		}
	}

	// Auto-prompt when the user is interactive and didn't pre-specify any
	// override on the command line. Skipped on a pipe (CI, demos) so scripts
	// don't block on stdin. Explicit --no-prompt opt-out for users who want
	// "use manifest defaults, no questions" even on a TTY.
	if !noPrompt && rank == "" && len(quotaRaw) == 0 && len(volumes) == 0 && stdinIsTTY() {
		pRank, pQuota, pVols, perr := promptHireOverrides(os.Stdin, os.Stderr, refInput)
		if perr != nil {
			fmt.Fprintf(os.Stderr, "hire: %v\n", perr)
			os.Exit(2)
		}
		rank, quotaRaw, volumes = pRank, pQuota, pVols
	}

	c := mustDial(ctx)
	defer c.Close()

	r, _, err := hireOneAgent(ctx, c, roomID, refInput, rank, quotaRaw, volumes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hire: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("hired %s (rank=%s)\n", r.Member.ImageName, r.Member.Rank)
}

// cmdHireFile implements `hive hire -f <hivefile-or-url> [--room <name>]`.
// It auto-creates the Room and hires every Agent declared in the Hivefile.
// Stdout receives only the RoomID so callers can do ROOM=$(hive hire -f ...).
func cmdHireFile(ctx context.Context, args []string) {
	var src, roomOverride string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-f" || a == "--file":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "hire: -f requires a hivefile path or URL")
				os.Exit(2)
			}
			if src != "" {
				fmt.Fprintln(os.Stderr, "hire: -f given more than once")
				os.Exit(2)
			}
			src = args[i+1]
			i++
		case a == "--room":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "hire: --room requires a non-empty name")
				os.Exit(2)
			}
			roomOverride = args[i+1]
			if roomOverride == "" {
				fmt.Fprintln(os.Stderr, "hire: --room requires a non-empty name")
				os.Exit(2)
			}
			i++
		case a == "--rank", a == "--quota", a == "--volume":
			fmt.Fprintf(os.Stderr, "hire: %s is per-agent and belongs in the Hivefile, not on `hive hire -f`\n", a)
			os.Exit(2)
		case a == "--no-prompt", a == "--non-interactive":
			// no-op: declarative -f mode never prompts. Accept the flag silently
			// so a wrapping script can pass it unconditionally without forking
			// per-mode arg lists.
		default:
			fmt.Fprintf(os.Stderr, "hire: unknown argument %q\n", a)
			os.Exit(2)
		}
	}
	if src == "" {
		fmt.Fprintln(os.Stderr, "hire: -f requires a hivefile path or URL")
		os.Exit(2)
	}

	hfPath := src
	if looksRemoteRef(src) {
		tmp, err := fetchHivefileToTemp(ctx, src)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hire: fetch hivefile: %v\n", err)
			os.Exit(1)
		}
		defer os.Remove(tmp)
		hfPath = tmp
	}

	hf, err := hivefile.Load(hfPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hire: %v\n", err)
		os.Exit(1)
	}

	c := mustDial(ctx)
	defer c.Close()

	// 1. Init Room.
	roomName := hf.Room
	if roomOverride != "" {
		roomName = roomOverride
	}
	raw, err := c.Call(ctx, ipc.MethodRoomInit, ipc.RoomInitParams{Name: roomName})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hire/init: %v\n", err)
		os.Exit(1)
	}
	var init ipc.RoomInitResult
	_ = json.Unmarshal(raw, &init)
	fmt.Fprintf(os.Stderr, "  room %s created\n", init.RoomID)

	// 2. Hire each declared Agent.
	for _, a := range hf.Agents {
		var quotaRaw json.RawMessage
		if len(a.Quota) > 0 {
			b, err := json.Marshal(a.Quota)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hire/quota %s: %v\n", a.Image, err)
				os.Exit(1)
			}
			quotaRaw = b
		}
		var vols []ipc.VolumeMountRef
		for _, v := range a.Volumes {
			vols = append(vols, ipc.VolumeMountRef{
				Name: v.Name, Mode: v.Mode, Mountpoint: v.Mountpoint,
			})
		}

		_, localRef, err := hireOneAgent(ctx, c, init.RoomID, a.Image, a.Rank, quotaRaw, vols)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hire %s: %v\n", a.Image, err)
			os.Exit(1)
		}
		display := a.Image
		if display != localRef {
			display = fmt.Sprintf("%s (= %s)", a.Image, localRef)
		}
		fmt.Fprintf(os.Stderr, "  hired %s\n", display)
	}

	// 3. Print RoomID to stdout so shell scripts can capture it.
	fmt.Println(init.RoomID)
}
