package daemon

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/anne-x/hive/internal/image"
)

// skillRunnerEntry is the filename used for the hive-skill-runner binary
// when it's linked into a skill Image. Double-underscore avoids collisions
// with any plausible Agent-authored file.
const skillRunnerEntry = "__hive_runner__"

// skillRunnerBin locates the hive-skill-runner binary on the host.
// Resolution order:
//  1. $HIVE_RUNNER_DIR/hive-skill-runner
//  2. <dir-of-hived>/hive-skill-runner
//
// Returning a non-existent path is allowed here; prepareSkillImage stats
// the file and surfaces a helpful error if it's missing.
func (d *Daemon) skillRunnerBin() (string, error) {
	if dir := os.Getenv("HIVE_RUNNER_DIR"); dir != "" {
		return filepath.Join(dir, "hive-skill-runner"), nil
	}
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(self), "hive-skill-runner"), nil
}

// prepareSkillImage makes a kind=skill Image runnable:
//   - hardlinks (or copies) hive-skill-runner into <img.Dir>/__hive_runner__,
//     so it appears at /app/__hive_runner__ inside the sandbox
//   - returns a shallow clone of *img with Entry rewritten to the runner
//   - returns env vars the runner needs (HIVE_SKILL_PATH/MODEL/TOOLS)
//
// For kind=binary (or any non-skill), returns img unchanged and nil env.
func (d *Daemon) prepareSkillImage(img *image.Image) (*image.Image, []string, error) {
	if img.Manifest.Kind != image.KindSkill {
		return img, nil, nil
	}

	runner, err := d.skillRunnerBin()
	if err != nil {
		return nil, nil, err
	}
	if _, err := os.Stat(runner); err != nil {
		return nil, nil, fmt.Errorf(
			"hive-skill-runner not found at %s (set HIVE_RUNNER_DIR or rebuild with `make build`): %w",
			runner, err)
	}

	dst := filepath.Join(img.Dir, skillRunnerEntry)
	if err := installRunner(runner, dst); err != nil {
		return nil, nil, fmt.Errorf("install skill runner: %w", err)
	}

	// Shallow clone so we don't mutate the stored Image's Entry.
	prepared := *img
	prepared.Manifest.Entry = skillRunnerEntry

	tools := strings.Join(img.Manifest.Tools, ",")
	if tools == "" {
		// Default: allow everything the Rank allows — final gate is still Rank.
		tools = "net,fs,peer,llm"
	}

	env := []string{
		"HIVE_SKILL_PATH=/app/" + img.Manifest.Skill,
		"HIVE_SKILL_MODEL=" + img.Manifest.Model,
		"HIVE_SKILL_TOOLS=" + tools,
	}
	return &prepared, env, nil
}

// installRunner makes `src` available at `dst`. Prefers hardlink (zero disk
// cost) and falls back to copy if src and dst live on different filesystems.
// Idempotent: a second call with the same paths is a no-op.
func installRunner(src, dst string) error {
	if existing, err := os.Stat(dst); err == nil {
		source, serr := os.Stat(src)
		if serr == nil && os.SameFile(existing, source) {
			return nil // already linked
		}
		// Stale or different file at dst — remove so we can relink.
		if err := os.Remove(dst); err != nil {
			return err
		}
	}
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	// Cross-device or other Link failure: fall back to copy.
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
