package daemon

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/anne-x/hive/internal/image"
)

// runnerEntry is the filename used for whichever built-in runner binary
// is linked into a non-binary Image. Double-underscore avoids collisions
// with any plausible Agent-authored file.
const runnerEntry = "__hive_runner__"

// runnerBins maps Kind → built-in runner binary name that hived locates
// alongside itself (or via HIVE_RUNNER_DIR). Adding a new kind is just
// one entry here + a case in prepareImageByKind.
var runnerBins = map[string]string{
	image.KindSkill:    "hive-skill-runner",
	image.KindWorkflow: "hive-workflow-runner",
}

// runnerBin locates a runner binary on the host. Resolution order:
//  1. $HIVE_RUNNER_DIR/<name>
//  2. <dir-of-hived>/<name>
//
// Returning a non-existent path is fine; prepareImageByKind stats it and
// surfaces a clear error if missing.
func (d *Daemon) runnerBin(name string) (string, error) {
	if dir := os.Getenv("HIVE_RUNNER_DIR"); dir != "" {
		return filepath.Join(dir, name), nil
	}
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(self), name), nil
}

// prepareImageByKind is the single entry point handleAgentHire calls.
// For kind=binary (or unknown) it returns the image untouched. For a
// runner-driven kind (skill, workflow) it hardlinks the runner into the
// image dir, rewrites Entry, and returns the env vars the runner needs.
func (d *Daemon) prepareImageByKind(img *image.Image) (*image.Image, []string, error) {
	switch img.Manifest.Kind {
	case image.KindSkill:
		return d.prepareSkillImage(img)
	case image.KindWorkflow:
		return d.prepareWorkflowImage(img)
	default:
		return img, nil, nil
	}
}

// prepareSkillImage: link hive-skill-runner + set HIVE_SKILL_PATH + shared env.
func (d *Daemon) prepareSkillImage(img *image.Image) (*image.Image, []string, error) {
	prepared, err := d.linkRunner(img, image.KindSkill)
	if err != nil {
		return nil, nil, err
	}
	env := []string{"HIVE_SKILL_PATH=/app/" + img.Manifest.Skill}
	env = append(env, sharedRunnerEnv(img)...)
	return prepared, env, nil
}

// prepareWorkflowImage: link hive-workflow-runner; set exactly one of
// HIVE_WORKFLOW_PATH (static) or HIVE_PLANNER_PATH (LLM) + shared env.
func (d *Daemon) prepareWorkflowImage(img *image.Image) (*image.Image, []string, error) {
	prepared, err := d.linkRunner(img, image.KindWorkflow)
	if err != nil {
		return nil, nil, err
	}
	var env []string
	switch {
	case img.Manifest.Workflow != "":
		env = append(env, "HIVE_WORKFLOW_PATH=/app/"+img.Manifest.Workflow)
	case img.Manifest.Planner != "":
		env = append(env, "HIVE_PLANNER_PATH=/app/"+img.Manifest.Planner)
	default:
		return nil, nil, fmt.Errorf("kind=workflow but neither workflow: nor planner: set")
	}
	env = append(env, sharedRunnerEnv(img)...)
	return prepared, env, nil
}

// sharedRunnerEnv: env vars both runner kinds read.
func sharedRunnerEnv(img *image.Image) []string {
	return []string{
		"HIVE_MODEL=" + img.Manifest.Model,
		"HIVE_TOOLS=" + strings.Join(img.Manifest.Tools, ","),
	}
}

// linkRunner hardlinks the runner binary into the image dir and returns
// a shallow clone of *img with Entry rewritten to runnerEntry.
func (d *Daemon) linkRunner(img *image.Image, kind string) (*image.Image, error) {
	binName, ok := runnerBins[kind]
	if !ok {
		return nil, fmt.Errorf("no runner registered for kind=%s", kind)
	}
	runner, err := d.runnerBin(binName)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(runner); err != nil {
		return nil, fmt.Errorf(
			"%s not found at %s (set HIVE_RUNNER_DIR or rebuild with `make build`): %w",
			binName, runner, err)
	}
	dst := filepath.Join(img.Dir, runnerEntry)
	if err := installRunner(runner, dst); err != nil {
		return nil, fmt.Errorf("install runner: %w", err)
	}
	prepared := *img
	prepared.Manifest.Entry = runnerEntry
	return &prepared, nil
}

// installRunner makes `src` available at `dst`. Prefers hardlink (zero
// disk cost) and falls back to copy across different filesystems.
// Idempotent: a second call with identical source + dest is a no-op.
func installRunner(src, dst string) error {
	if existing, err := os.Stat(dst); err == nil {
		source, serr := os.Stat(src)
		if serr == nil && os.SameFile(existing, source) {
			return nil
		}
		if err := os.Remove(dst); err != nil {
			return err
		}
	}
	if err := os.Link(src, dst); err == nil {
		return nil
	}
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
