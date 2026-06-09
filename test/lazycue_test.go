package test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	lazycue "github.com/boldsoftware/shelley/lazycue"

	"shelley.exe.dev/claudetool"
	"shelley.exe.dev/db"
	"shelley.exe.dev/server"
)

// LazyCue self-healing browser tests for the Shelley /new UI.
//
// Each test is a plain-English description run through a package-level
// lazycue.Harness. The harness hashes the description, looks up a cached DSL
// script in shelley/ui/lazycue/.lazycue/, and executes it without an LLM. On a
// cache miss or a mechanical failure it spawns an agent to generate/heal the
// script and writes it back to the cache. The descriptions are the source of
// truth: they live here, in the Go test, not in a separate data file.
//
// These need a real browser (headless-shell) and, on a cache miss/heal, an
// LLM. To keep the default `go test ./...` fast and hermetic they only run when
// LAZYCUE_INTEGRATION is set (the dedicated CI step sets it); they skip cleanly
// when no browser is available.
//
// TestMain boots one in-process predictable-mode Shelley server shared by all
// the tests, then (when artifacts are requested) writes an aggregate HTML
// report and JSON cache-stats summary. Optional env vars, set by CI to feed the
// reporting scripts:
//
//	LAZYCUE_ARTIFACT_DIR  write per-step screenshots + an HTML report here
//	LAZYCUE_SUMMARY       write a machine-readable JSON cache-stats summary here

// app is the shared harness. Its BaseURL is filled in by TestMain once the
// server is listening.
var app *lazycue.Harness

func TestMain(m *testing.M) {
	if os.Getenv("LAZYCUE_INTEGRATION") == "" {
		// Tests below all skip; run them so `go test` reports them as skipped.
		os.Exit(m.Run())
	}

	ts, cleanup := startPredictableServer()
	defer cleanup()

	app = lazycue.New(lazycue.Options{
		BaseURL:     ts.URL,
		CacheDir:    filepath.Join("..", "ui", "lazycue", ".lazycue"),
		Verbose:     true,
		ArtifactDir: os.Getenv("LAZYCUE_ARTIFACT_DIR"),
	})

	code := m.Run()

	// Emit the reporting artifacts CI surfaces (HTML report + JSON summary).
	results := app.Results()
	if dir := os.Getenv("LAZYCUE_ARTIFACT_DIR"); dir != "" && len(results) > 0 {
		if err := lazycue.WriteReport(dir, results); err != nil {
			slog.Warn("lazycue: write report", "error", err)
		}
	}
	if path := os.Getenv("LAZYCUE_SUMMARY"); path != "" && len(results) > 0 {
		if err := lazycue.WriteSummary(path, results); err != nil {
			slog.Warn("lazycue: write summary", "error", err)
		}
	}

	os.Exit(code)
}

func lazyTest(t *testing.T, description string) {
	t.Helper()
	if os.Getenv("LAZYCUE_INTEGRATION") == "" {
		t.Skip("set LAZYCUE_INTEGRATION=1 to run the LazyCue browser integration tests")
	}
	app.Test(t, description)
}

func TestNewPageSmoke(t *testing.T) {
	lazyTest(t, `Navigate to /new. The page title should be "Shelley Agent". The message input (a textarea with data-testid "message-input") should be visible and initially empty, and the send button (data-testid "send-button") should be visible but disabled while the input is empty.`)
}

func TestNewPageAccessibility(t *testing.T) {
	lazyTest(t, `Navigate to /new. The message input (data-testid "message-input") should have an aria-label of "Message input" and a non-empty placeholder attribute. The send button (data-testid "send-button") should have an aria-label of "Send message".`)
}

func TestNewPageSendEnables(t *testing.T) {
	lazyTest(t, `Navigate to /new. Type the text "hello world" into the message input (data-testid "message-input"). After typing, the send button (data-testid "send-button") should become enabled.`)
}

// startPredictableServer boots a Shelley server in predictable mode backed by a
// temp DB and the embedded UI. It returns the test server and a cleanup func.
func startPredictableServer() (*httptest.Server, func()) {
	tempDB, err := os.MkdirTemp("", "lazycue-db-")
	if err != nil {
		panic(err)
	}
	database, err := db.New(db.Config{DSN: filepath.Join(tempDB, "test.db")})
	if err != nil {
		panic(err)
	}
	if err := database.Migrate(context.Background()); err != nil {
		panic(err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	llmManager := server.NewLLMServiceManager(newPredictableLLMConfig(logger))
	svr := server.NewServer(database, llmManager, claudetool.ToolSetConfig{}, logger, true, "predictable", "")

	// RegisterRoutes wires up the SPA-aware static handler for the embedded UI
	// (so /new and its assets resolve) plus the API endpoints.
	mux := http.NewServeMux()
	svr.RegisterRoutes(mux)

	ts := httptest.NewServer(mux)
	return ts, func() {
		ts.Close()
		database.Close()
		os.RemoveAll(tempDB)
	}
}
