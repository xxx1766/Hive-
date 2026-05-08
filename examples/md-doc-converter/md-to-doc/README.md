# md-to-doc

Real Markdown → `.docx` converter. `kind: binary`, written in Go, uses
[goldmark](https://github.com/yuin/goldmark) for Markdown parsing and
[godocx](https://github.com/gomutex/godocx) for `.docx` emission. No
shell-out and no host-side dependencies (no pandoc, no LibreOffice).

## Input shape (task/run)

```jsonc
{
  // Source — provide ONE of:
  "markdown": "# Hello\n\nThis is **bold**.",
  // OR:
  "input_files": [{"volume": "<vol>", "path": "memory/<key>"}],

  // Required output target:
  "output_volume":   "<vol>",
  "output_subdir":   "<rel/path>",   // optional; "" → volume root
  "output_filename": "doc.docx",      // optional; default doc-<unix>.docx

  // Optional — apply this template's styles to the rendered output:
  "template": {"volume": "<vol>", "path": "memory/<key>"}
}
```

## Output (task/done)

```jsonc
{
  "format":   "docx",
  "saved_to": "<vol>:memory/<key>",
  "bytes":    <int>
}
```

The bytes are written into the named Volume's `memory/` subdir via
`memory_put`. Open the file in Word / LibreOffice / Pages.

## Supported Markdown

| Construct | Renders as |
|---|---|
| `# H1` … `###### H6` | Paragraph with style `Heading1` … `Heading6` (template can restyle) |
| Paragraph | Default-styled paragraph |
| `**bold**` | Run with bold |
| `*italic*` | Run with italic |
| `***bold italic***` | Run with both |
| `` `inline code` `` | Run with character style `Code` (template-defined; bare style otherwise) |
| ` ```code blocks``` ` | Per-line paragraphs with style `Code` |
| `- bullet` | Paragraph with style `ListBullet` |
| `1. numbered` | Paragraph with style `ListNumber` |
| `---` thematic break | Visual `———` paragraph |
| Plain text + line breaks | Preserved as run text |

## Not yet supported (silently degraded)

- Tables — text content kept; table structure not rendered.
- Images — replaced with `[image: <alt>]` placeholder.
- Hyperlinks — link text kept; URL dropped (no relationship part).
- Nested lists — flattened to a single level.
- Raw HTML / HTML blocks — skipped.
- Mustache-style `{{placeholder}}` template fill — not this round.

## Template style reuse

Provide `template: {"volume":"...","path":"memory/template.docx"}` to
have your `.docx` template's styles (heading colours, fonts, theme)
take effect on the output. Mechanism: the agent opens the template as
the base document and **appends** rendered content to its existing
body. So:

- A template with an **empty body** (just stylesheet) → output is
  pure rendered Markdown styled per template.
- A template with a **cover sheet** → output is "cover sheet, then
  rendered Markdown". The cover stays.

If you want placeholder substitution (`{{title}}` etc.), file a follow-up.

## Known caveat: input_files paths must live under `memory/`

`memory_get` (the agent's read API) is rooted at `<volume>/memory/`,
so `input_files[0].path` should look like `memory/<key>` (or just
`<key>`; we strip a leading `memory/` automatically).

Files uploaded via the SPA's Files picker land under
`<volume>/uploads/` and are NOT readable by `memory_get`. If you've
uploaded a `.md` to a Volume's uploads/ via the UI and want this
agent to read it, copy or move it to memory/ first — there's no
runtime translation in this version. A "write directly to memory/"
upload mode is a planned follow-up.

## Prerequisites

- The target Volume (`output_volume`) must exist before the run —
  create it with `hive volume create <name>` or via the UI's
  Volumes tab. `memory_put` errors with a clear message otherwise.
- Rank `staff` or higher (binary agents with `tools: [memory]` need
  staff to access memory_put).

## Examples

Inline source:
```bash
ROOM=$(hive hire -f hivefiles/md-doc-single.yaml | tail -1)
hive volume create md-out
hive run "$ROOM" --target md-to-doc '{
  "markdown": "# Hello\n\nThis is **bold**.\n\n- one\n- two",
  "output_volume": "md-out",
  "output_filename": "hello.docx"
}'
ls ~/.hive/volumes/md-out/memory/  # → hello.docx
```

From a file already in a Volume:
```bash
# Upload first.md via the UI or CLI; then:
hive run "$ROOM" --target md-to-doc '{
  "input_files": [{"volume": "md-out", "path": "uploads/first.md"}],
  "output_volume": "md-out"
}'
```

With a template:
```bash
# Upload your template.docx first; then:
hive run "$ROOM" --target md-to-doc '{
  "markdown": "# Heading\n\nBody.",
  "template": {"volume": "md-out", "path": "uploads/template.docx"},
  "output_volume": "md-out",
  "output_filename": "styled.docx"
}'
```
