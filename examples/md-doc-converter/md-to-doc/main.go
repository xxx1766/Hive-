// md-to-doc is a Hive binary Agent that converts Markdown source into
// a real Microsoft Word .docx file. The conversion happens in-process
// via goldmark (Markdown → AST) + godocx (AST → docx bytes), with no
// shell-out and no external dependencies on the host.
//
// Input shapes (provide either `markdown` or `input_files[0]`):
//
//	{
//	  "markdown": "<literal text>",
//
//	  "input_files": [{"volume":"<vol>","path":"memory/<key>"}],
//
//	  // required output target:
//	  "output_volume":   "<vol>",
//	  "output_subdir":   "<rel/path>",  // optional; "" → root
//	  "output_filename": "doc.docx",    // optional; default doc-<unix>.docx
//
//	  // optional template — its styles will be applied to the rendered output:
//	  "template": {"volume":"<vol>","path":"memory/<key>"}
//	}
//
// Output (task/done):
//
//	{
//	  "format":   "docx",
//	  "saved_to": "<vol>:memory/<key>",
//	  "bytes":    <int>
//	}
//
// Template semantics: when `template` is supplied, the agent opens the
// template file as a base document and APPENDS rendered paragraphs to
// its body. The user's template defines headings/fonts/colours via its
// stylesheet — godocx emits style references (e.g. <w:pStyle val=
// "Heading1"/>) so the template's definitions take effect. Mustache-
// style {{placeholder}} replacement is NOT implemented in this version.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	hive "github.com/anne-x/hive/sdk/go"
)

// fileRef points at a single file inside a Volume. The path is the
// posix-style relative location returned by /api/volumes/{name}/tree.
// For memory_get we strip a leading "memory/" since the SDK's `key`
// is rooted at the volume's memory/ subdir (not the volume root).
type fileRef struct {
	Volume string `json:"volume"`
	Path   string `json:"path"`
}

// taskInput is the full shape we accept on task/run.
type taskInput struct {
	Markdown       string    `json:"markdown,omitempty"`
	InputFiles     []fileRef `json:"input_files,omitempty"`
	OutputVolume   string    `json:"output_volume"`
	OutputSubdir   string    `json:"output_subdir,omitempty"`
	OutputFilename string    `json:"output_filename,omitempty"`
	Template       *fileRef  `json:"template,omitempty"`
}

func main() {
	a := hive.MustConnect()
	defer a.Close()

	for {
		select {
		case task, ok := <-a.Tasks():
			if !ok {
				return
			}
			handleTask(a, task)
		case <-a.Done():
			return
		}
	}
}

// handleTask drives one task/run end-to-end. Failures land as task.Fail
// so the conversation transcript shows a clear error message.
func handleTask(a *hive.Agent, task *hive.Task) {
	var in taskInput
	if err := json.Unmarshal(task.Input, &in); err != nil {
		_ = task.Fail(400, "invalid input JSON: "+err.Error())
		return
	}
	if in.OutputVolume == "" {
		_ = task.Fail(400, "output_volume is required")
		return
	}
	a.Log("info", "md-to-doc starting", map[string]any{
		"has_markdown":   in.Markdown != "",
		"input_files":    len(in.InputFiles),
		"has_template":   in.Template != nil,
		"output_volume":  in.OutputVolume,
		"output_subdir":  in.OutputSubdir,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Resolve the source markdown. `markdown` field wins; otherwise pull
	// from the first input_files entry via memory_get.
	var src []byte
	switch {
	case in.Markdown != "":
		src = []byte(in.Markdown)
	case len(in.InputFiles) > 0:
		ref := in.InputFiles[0]
		key := strings.TrimPrefix(ref.Path, "memory/")
		bytes, exists, err := a.MemoryGet(ctx, ref.Volume, key)
		if err != nil {
			_ = task.Fail(500, fmt.Sprintf("memory_get %s/%s: %v", ref.Volume, key, err))
			return
		}
		if !exists {
			_ = task.Fail(404, fmt.Sprintf("input file not found: %s/%s", ref.Volume, ref.Path))
			return
		}
		src = bytes
	default:
		_ = task.Fail(400, "either `markdown` or `input_files[0]` is required")
		return
	}

	// Optional template — fetched as bytes, written to a tempfile so
	// godocx.OpenDocument can load it (the library's API is path-only).
	templatePath := ""
	if in.Template != nil {
		key := strings.TrimPrefix(in.Template.Path, "memory/")
		bytes, exists, err := a.MemoryGet(ctx, in.Template.Volume, key)
		if err != nil {
			_ = task.Fail(500, fmt.Sprintf("memory_get template %s/%s: %v", in.Template.Volume, key, err))
			return
		}
		if !exists {
			_ = task.Fail(404, fmt.Sprintf("template not found: %s/%s", in.Template.Volume, in.Template.Path))
			return
		}
		tmp, err := os.CreateTemp("", "md-to-doc-template-*.docx")
		if err != nil {
			_ = task.Fail(500, "tempfile: "+err.Error())
			return
		}
		_, _ = tmp.Write(bytes)
		_ = tmp.Close()
		templatePath = tmp.Name()
		defer os.Remove(templatePath)
	}

	// Render. Returns the docx bytes ready to memory_put.
	out, err := renderMarkdownToDocx(src, templatePath)
	if err != nil {
		_ = task.Fail(500, "render: "+err.Error())
		return
	}

	// Output key. Default file name uses the unix epoch so back-to-back
	// runs don't overwrite each other.
	filename := in.OutputFilename
	if filename == "" {
		filename = fmt.Sprintf("doc-%d.docx", time.Now().Unix())
	}
	if !strings.HasSuffix(filename, ".docx") {
		filename += ".docx"
	}
	key := filename
	if in.OutputSubdir != "" {
		key = filepath.ToSlash(filepath.Join(in.OutputSubdir, filename))
	}

	if err := a.MemoryPut(ctx, in.OutputVolume, key, out); err != nil {
		_ = task.Fail(500, fmt.Sprintf("memory_put %s/%s: %v", in.OutputVolume, key, err))
		return
	}

	a.Log("info", "md-to-doc done", map[string]any{
		"saved_to": fmt.Sprintf("%s:memory/%s", in.OutputVolume, key),
		"bytes":    len(out),
	})
	_ = task.Reply(map[string]any{
		"format":   "docx",
		"saved_to": fmt.Sprintf("%s:memory/%s", in.OutputVolume, key),
		"bytes":    len(out),
	})
}
