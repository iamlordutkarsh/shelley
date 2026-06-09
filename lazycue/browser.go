package lazycue

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// Browser wraps a headless Chrome instance via chromedp.
type Browser struct {
	allocCancel context.CancelFunc
	ctxCancel   context.CancelFunc
	ctx         context.Context

	// screenshotSink, when non-nil, is invoked after every executed step with
	// the step index and a PNG screenshot of the page. Used to capture a
	// visual trace of a test run. Errors capturing screenshots are ignored.
	screenshotSink func(stepIndex int, action string, png []byte)
}

// SetScreenshotSink installs a callback invoked after each executed step with
// a PNG screenshot. Pass nil to disable. Used to produce a visual trace.
func (b *Browser) SetScreenshotSink(sink func(stepIndex int, action string, png []byte)) {
	b.screenshotSink = sink
}

// NewBrowser launches a headless Chrome instance with Pixel 5 viewport (393x851).
func NewBrowser(parentCtx context.Context) (*Browser, error) {
	opts := append(
		chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-dbus", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.WindowSize(393, 851),
	)

	allocCtx, allocCancel := chromedp.NewExecAllocator(parentCtx, opts...)
	ctx, ctxCancel := chromedp.NewContext(allocCtx)

	// Set device metrics for Pixel 5 viewport.
	if err := chromedp.Run(
		ctx,
		emulation.SetDeviceMetricsOverride(393, 851, 2.75, true),
	); err != nil {
		ctxCancel()
		allocCancel()
		return nil, fmt.Errorf("set viewport: %w", err)
	}

	return &Browser{
		allocCancel: allocCancel,
		ctxCancel:   ctxCancel,
		ctx:         ctx,
	}, nil
}

// Close shuts down the browser.
func (b *Browser) Close() {
	if b.ctxCancel != nil {
		b.ctxCancel()
	}
	if b.allocCancel != nil {
		b.allocCancel()
	}
}

// Context returns the browser's chromedp context.
func (b *Browser) Context() context.Context {
	return b.ctx
}

// Screenshot captures a full-page PNG screenshot.
func (b *Browser) Screenshot(ctx context.Context) ([]byte, error) {
	var buf []byte
	if err := chromedp.Run(b.ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		buf, err = page.CaptureScreenshot().
			WithFormat(page.CaptureScreenshotFormatPng).
			WithCaptureBeyondViewport(false).
			Do(ctx)
		return err
	})); err != nil {
		return nil, err
	}
	return buf, nil
}

// ExecuteSteps runs a sequence of DSL steps against the browser.
// It stops on the first failure and returns results for all attempted steps.
func (b *Browser) ExecuteSteps(ctx context.Context, baseURL string, steps []Step) ([]StepResult, error) {
	var results []StepResult
	for i, step := range steps {
		start := time.Now()
		err := b.executeStep(ctx, baseURL, step)
		dur := time.Since(start)
		sr := StepResult{
			Action:   step.Action,
			Summary:  StepSummary(step),
			Pass:     err == nil,
			Duration: dur,
		}
		if err != nil {
			sr.Error = err.Error()
		}
		if b.screenshotSink != nil {
			if png, sErr := b.Screenshot(ctx); sErr == nil {
				b.screenshotSink(i, step.Action, png)
			}
		}
		results = append(results, sr)
		if err != nil {
			return results, fmt.Errorf("step %d (%s): %w", i, step.Action, err)
		}
	}
	return results, nil
}

func (b *Browser) executeStep(ctx context.Context, baseURL string, step Step) error {
	timeout := parseTimeout(step.Timeout, 10*time.Second)

	switch step.Action {
	case ActionNavigate:
		url := step.URL
		if !strings.HasPrefix(url, "http") {
			url = strings.TrimRight(baseURL, "/") + "/" + strings.TrimLeft(url, "/")
		}
		return chromedp.Run(b.ctx, chromedp.Navigate(url))

	case ActionWaitVisible:
		return b.pollJS(ctx, timeout, fmt.Sprintf(
			`(function() {
				const el = document.querySelector(%q);
				if (!el) return false;
				const style = window.getComputedStyle(el);
				return style.display !== 'none' && style.visibility !== 'hidden' && el.offsetParent !== null;
			})()`, step.Selector,
		))

	case ActionWaitHidden:
		return b.pollJS(ctx, timeout, fmt.Sprintf(
			`(function() {
				const el = document.querySelector(%q);
				if (!el) return true;
				const style = window.getComputedStyle(el);
				return style.display === 'none' || style.visibility === 'hidden' || el.offsetParent === null;
			})()`, step.Selector,
		))

	case ActionWaitText:
		return b.pollJS(ctx, timeout, fmt.Sprintf(
			`(document.body.textContent || '').includes(%q)`, step.Text,
		))

	case ActionWaitTextGone:
		return b.pollJS(ctx, timeout, fmt.Sprintf(
			`!(document.body.textContent || '').includes(%q)`, step.Text,
		))

	case ActionFill:
		return b.fill(ctx, step.Selector, step.Value)

	case ActionClick:
		return chromedp.Run(b.ctx, chromedp.Click(step.Selector, chromedp.ByQuery))

	case ActionPressKey:
		return chromedp.Run(b.ctx, chromedp.KeyEvent(step.Key))

	case ActionScreenshot:
		// Just take a screenshot, ignore the bytes (used for side effects in agent)
		_, err := b.Screenshot(ctx)
		return err

	case ActionEval:
		var result interface{}
		if err := chromedp.Run(b.ctx, chromedp.Evaluate(step.Expression, &result)); err != nil {
			return err
		}
		if step.Expect != "" {
			got := fmt.Sprintf("%v", result)
			if got != step.Expect {
				return fmt.Errorf("eval: expected %q, got %q", step.Expect, got)
			}
		}
		return nil

	case ActionAssertVisible:
		var visible bool
		if err := chromedp.Run(b.ctx, chromedp.Evaluate(fmt.Sprintf(
			`(function() {
				const el = document.querySelector(%q);
				if (!el) return false;
				const style = window.getComputedStyle(el);
				return style.display !== 'none' && style.visibility !== 'hidden' && el.offsetParent !== null;
			})()`, step.Selector,
		), &visible)); err != nil {
			return err
		}
		if !visible {
			return fmt.Errorf("assert_visible: element %q not visible", step.Selector)
		}
		return nil

	case ActionAssertNotVisible:
		var visible bool
		if err := chromedp.Run(b.ctx, chromedp.Evaluate(fmt.Sprintf(
			`(function() {
				const el = document.querySelector(%q);
				if (!el) return false;
				const style = window.getComputedStyle(el);
				return style.display !== 'none' && style.visibility !== 'hidden' && el.offsetParent !== null;
			})()`, step.Selector,
		), &visible)); err != nil {
			return err
		}
		if visible {
			return fmt.Errorf("assert_not_visible: element %q is visible", step.Selector)
		}
		return nil

	case ActionAssertText:
		var got string
		if err := chromedp.Run(b.ctx, chromedp.TextContent(step.Selector, &got, chromedp.ByQuery)); err != nil {
			return fmt.Errorf("assert_text: %w", err)
		}
		got = strings.TrimSpace(got)
		if got != step.Text {
			return fmt.Errorf("assert_text: expected %q, got %q", step.Text, got)
		}
		return nil

	case ActionAssertTextContains:
		var got string
		if err := chromedp.Run(b.ctx, chromedp.TextContent(step.Selector, &got, chromedp.ByQuery)); err != nil {
			return fmt.Errorf("assert_text_contains: %w", err)
		}
		if !strings.Contains(got, step.Text) {
			return fmt.Errorf("assert_text_contains: %q not found in %q", step.Text, got)
		}
		return nil

	case ActionAssertAttribute:
		var got string
		if err := chromedp.Run(b.ctx, chromedp.AttributeValue(step.Selector, step.Attribute, &got, nil, chromedp.ByQuery)); err != nil {
			return fmt.Errorf("assert_attribute: %w", err)
		}
		if got != step.Value {
			return fmt.Errorf("assert_attribute %q: expected %q, got %q", step.Attribute, step.Value, got)
		}
		return nil

	case ActionAssertURL:
		var got string
		if err := chromedp.Run(b.ctx, chromedp.Location(&got)); err != nil {
			return err
		}
		if step.Value != "" && got != step.Value {
			return fmt.Errorf("assert_url: expected %q, got %q", step.Value, got)
		}
		if step.Text != "" && !strings.Contains(got, step.Text) {
			return fmt.Errorf("assert_url: %q not found in %q", step.Text, got)
		}
		return nil

	case ActionAssertTitle:
		var got string
		if err := chromedp.Run(b.ctx, chromedp.Title(&got)); err != nil {
			return err
		}
		if got != step.Text {
			return fmt.Errorf("assert_title: expected %q, got %q", step.Text, got)
		}
		return nil

	case ActionAssertCount:
		var count int
		if err := chromedp.Run(b.ctx, chromedp.Evaluate(fmt.Sprintf(
			`document.querySelectorAll(%q).length`, step.Selector,
		), &count)); err != nil {
			return fmt.Errorf("assert_count: %w", err)
		}
		if count != step.Count {
			return fmt.Errorf("assert_count: expected %d elements matching %q, got %d", step.Count, step.Selector, count)
		}
		return nil

	case ActionSleep:
		d := parseTimeout(step.Timeout, 1*time.Second)
		time.Sleep(d)
		return nil

	default:
		return fmt.Errorf("unknown action: %q", step.Action)
	}
}

// fill sets a value on an input/textarea with React-compatible event dispatching.
func (b *Browser) fill(ctx context.Context, selector, value string) error {
	// Determine if this is a textarea or input.
	js := fmt.Sprintf(`(function() {
		const el = document.querySelector(%q);
		if (!el) throw new Error('element not found: ' + %q);
		const tag = el.tagName.toLowerCase();
		const proto = tag === 'textarea' ? window.HTMLTextAreaElement.prototype : window.HTMLInputElement.prototype;
		const nativeSetter = Object.getOwnPropertyDescriptor(proto, 'value').set;
		nativeSetter.call(el, %q);
		el.dispatchEvent(new Event('input', { bubbles: true }));
		el.dispatchEvent(new Event('change', { bubbles: true }));
		return true;
	})()`, selector, selector, value)

	var result bool
	return chromedp.Run(b.ctx, chromedp.Evaluate(js, &result))
}

// pollJS polls a JS expression until it returns true or the timeout expires.
func (b *Browser) pollJS(ctx context.Context, timeout time.Duration, expr string) error {
	deadline := time.Now().Add(timeout)
	interval := 200 * time.Millisecond

	for {
		var result bool
		if err := chromedp.Run(b.ctx, chromedp.Evaluate(expr, &result)); err != nil {
			// JS errors during polling are not fatal — element might not exist yet
		} else if result {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("timeout after %s waiting for: %s", timeout, expr)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// parseTimeout parses a duration string like "10s", "5s", etc.
// Returns the default if the string is empty or unparseable.
func parseTimeout(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	return d
}
