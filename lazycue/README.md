# LazyCue

We may want Playwright/Selenium/Cypress/etc. tests, but we don't want
to maintain them. If we're being honest, we don't want to write them
either.

LazyCue is an experiment in "self-healing" browser automation tests. The
tests themselves are free form instructions, and an agent, at run time,
interprets those instructions. Then, the instructions are cached, and the
cached instructions are re-used over and over again. When that eventually
fails, an agent is invoked again to fix them up.

Since we, as an industry, are not entirely comfortable with CI tooling that
mutates git refs behind the scenes, the cache is kept as ordinary tracked
files in your repo: one JSON file per test description, living in a
`.lazycue/` directory next to your tests. They get committed like any other
source file.

## Quick Start

```bash
go run github.com/boldsoftware/shelley/lazycue/cmd/lazycue@latest \
  --base-url http://localhost:3000 \
  'Navigate to / and verify the page title is "My App". The login button should be visible.'
```

Or from Go tests:

```go
var app = lazycue.New(lazycue.Options{BaseURL: "http://localhost:3000"})

func TestHomepage(t *testing.T) {
    app.Test(t, `Navigate to / and verify the page title is "My App". The login button should be visible.`)
}
```

## Workflow of a Run

1. Hash description → look up `.lazycue/<hash>.json` next to your tests
2. If cached: execute DSL steps. If passes → done (fast path, ~1-2s)
3. If cached but fails mechanically: spawn LLM agent to fix → save new version
4. If cached but app is genuinely broken: **fail the test with explanation**
5. If not cached: spawn LLM agent to generate → save v1

### DSL

Cached tests are stored as JSON as arrays of steps, for example:

```json
[
  {"action": "navigate", "url": "/"},
  {"action": "assert_title", "text": "My App"},
  {"action": "wait_visible", "selector": "#login-button", "timeout": "10s"},
  {"action": "click", "selector": "#login-button"},
  {"action": "wait_text", "text": "Welcome back", "timeout": "10s"}
]
```

### Self-Healing vs Genuine Failures

The agent distinguishes between:
- **Mechanical failures**: wrong selectors, timing issues, missing waits → self-heals
- **Genuine failures**: app doesn't match the description → fails with explanation

The test description is the source of truth. If the description says "title should be X"
and the app shows "Y", that's a genuine failure.

## Usage

### CLI

```bash
# The CLI runs exactly one test, passed as a single description string.
go run github.com/boldsoftware/shelley/lazycue/cmd/lazycue@latest \
  --base-url http://localhost:3000 "Navigate to / and verify the title is My App"
```

To run a suite, drive `lazycue.Run` from a Go test (see below) or loop over the
CLI in a script.

### CI Workflow

In CI, the cache files generated or healed during a green build are committed
back to the repo (much like auto-formatting), so future runs hit the fast
path:

```bash
# Run tests (e.g. from a Go test suite); new/updated cache files land in
# .lazycue/. If CI passes, commit the cache files so future runs are fast:
git add .lazycue && git commit -m "Update LazyCue cache"
```

The cache files are clearly marked as machine-managed; don't edit them by hand.

### Go Tests

```go
package myapp_test

import (
    "testing"

    lazycue "github.com/boldsoftware/shelley/lazycue"
)

var app = lazycue.New(lazycue.Options{BaseURL: "http://localhost:3000"})

func TestLogin(t *testing.T) {
    app.Test(t, "Navigate to /login, fill email with user@test.com and password with secret, click Submit, verify the dashboard heading appears")
}

func TestHomepage(t *testing.T) {
    app.Test(t, "Navigate to / and verify the page title is My App")
}
```

`Test` calls `t.Fatal` on failure and logs each step result via `t.Log`.
The agent discovers app structure automatically via screenshots and `git grep`.

The `Harness` accumulates every result, so a `TestMain` can emit an aggregate
HTML report and JSON cache-stats summary after the suite finishes:

```go
func TestMain(m *testing.M) {
    code := m.Run()
    lazycue.WriteReport("/tmp/lazycue-artifacts", app.Results())
    lazycue.WriteSummary("/tmp/lazycue-summary.json", app.Results())
    os.Exit(code)
}
```

## Configuration

| Flag / Option | Default | Description |
|---------------|---------|-------------|
| `--base-url` | | App URL (**required**) |
| `--cache-dir` | `.lazycue` | Directory holding cache JSON files |
| `--artifact-dir` | | Write per-step screenshots + an HTML report (`index.html`) here |
| `--json` | | Write a machine-readable JSON cache-stats summary here |
| `--model` | `claude-sonnet-4-6` | LLM model |
| `--api-url` | `ANTHROPIC_BASE_URL` or `https://api.anthropic.com` | Anthropic API base URL |
| `--api-key` | `ANTHROPIC_API_KEY` | Anthropic API key |
| `--verbose` | false | Verbose output |

## How the Cache Works

Cached tests live in a `.lazycue/` directory next to your tests, as tracked
JSON files — one file per description. They show up in `git status` and
diffs, and you commit them like any other source file.

### Storage

When the agent generates or heals a test, it:

1. Serializes the DSL steps + metadata to JSON
2. Writes `.lazycue/<desc_hash>.json`, where `<desc_hash>` is the first 16 hex
   chars of the SHA-256 of the test description
3. Healing overwrites the same file, bumping `version` and setting `mode` to
   `"healed"`

Each file carries a `_README` banner marking it as machine-managed, so nobody
hand-edits it. To change behavior, edit the test description and re-run
LazyCue.

### Lookup

On each run, the tool reads `.lazycue/<desc_hash>.json`. If it exists, the
cached DSL is parsed and executed. If it's absent, the agent generates a fresh
test. No git, no network, no ancestry — just a file read.

### What's in a cache file

```json
{
  "_README": "This file is managed by LazyCue ... Do NOT edit by hand ...",
  "description": "Navigate to / and verify the page title is My App",
  "version": 1,
  "steps": [
    {"action": "navigate", "url": "/"},
    {"action": "assert_title", "text": "My App"}
  ],
  "metadata": {
    "created_at": "2025-06-07T...",
    "hostname": "ci-agent-1",
    "model": "claude-sonnet-4-6",
    "input_tokens": 12450,
    "output_tokens": 890,
    "estimated_cost_usd": 0.051,
    "git_sha": "abc123...",
    "mode": "generated"
  }
}
```

### Inspecting

```bash
# List all cached tests
ls .lazycue/

# View a cached test
jq . .lazycue/<hash>.json
```
