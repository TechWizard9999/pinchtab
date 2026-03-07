package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

const TargetTypePage = "page"

type elementMetrics struct {
	X      float64
	Y      float64
	Width  float64
	Height float64
}

// NavigatePage uses raw CDP Page.navigate + polls document.readyState for completion.
func NavigatePage(ctx context.Context, url string) error {
	err := chromedp.Run(ctx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			_, _, _, _, err := page.Navigate(url).Do(ctx)
			return err
		}),
	)
	if err != nil {
		return err
	}

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			var state string
			err = chromedp.Run(ctx,
				chromedp.Evaluate("document.readyState", &state),
			)
			if err == nil && (state == "interactive" || state == "complete") {
				return nil
			}
		}
	}
}

var ImageBlockPatterns = []string{
	"*.png", "*.jpg", "*.jpeg", "*.gif", "*.webp", "*.svg", "*.ico",
}

var MediaBlockPatterns = append(ImageBlockPatterns,
	"*.mp4", "*.webm", "*.ogg", "*.mp3", "*.wav", "*.flac", "*.aac",
)

// SetResourceBlocking uses Network.setBlockedURLs to block resources by URL pattern.
func SetResourceBlocking(ctx context.Context, patterns []string) error {
	return chromedp.Run(ctx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			if len(patterns) == 0 {
				return network.SetBlockedURLs([]string{}).Do(ctx)
			}
			return network.SetBlockedURLs(patterns).Do(ctx)
		}),
	)
}

func ClickByNodeID(ctx context.Context, nodeID int64) error {
	metrics, err := getElementCenter(ctx, nodeID)
	if err != nil || metrics.Width <= 0 || metrics.Height <= 0 {
		fallbackErr := fallbackJSClick(ctx, nodeID)
		if fallbackErr != nil {
			if err != nil {
				return fmt.Errorf("get element center: %w; js click fallback: %w", err, fallbackErr)
			}
			return fmt.Errorf("js click fallback: %w", fallbackErr)
		}
		return nil
	}

	return chromedp.Run(ctx,
		// Scroll element into view first
		chromedp.ActionFunc(func(ctx context.Context) error {
			return chromedp.FromContext(ctx).Target.Execute(ctx, "DOM.scrollIntoViewIfNeeded", map[string]any{"backendNodeId": nodeID}, nil)
		}),
		// Focus the element
		chromedp.ActionFunc(func(ctx context.Context) error {
			return chromedp.FromContext(ctx).Target.Execute(ctx, "DOM.focus", map[string]any{"backendNodeId": nodeID}, nil)
		}),
		// Mouse down at element center
		chromedp.ActionFunc(func(ctx context.Context) error {
			return chromedp.FromContext(ctx).Target.Execute(ctx, "Input.dispatchMouseEvent", map[string]any{
				"type":       "mousePressed",
				"button":     "left",
				"clickCount": 1,
				"x":          metrics.X, "y": metrics.Y,
			}, nil)
		}),
		// Mouse up at element center
		chromedp.ActionFunc(func(ctx context.Context) error {
			return chromedp.FromContext(ctx).Target.Execute(ctx, "Input.dispatchMouseEvent", map[string]any{
				"type":       "mouseReleased",
				"button":     "left",
				"clickCount": 1,
				"x":          metrics.X, "y": metrics.Y,
			}, nil)
		}),
	)
}

func fallbackJSClick(ctx context.Context, backendNodeID int64) error {
	objectID, err := resolveClickFallbackObjectID(ctx, backendNodeID)
	if err != nil {
		return err
	}

	return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		return chromedp.FromContext(ctx).Target.Execute(ctx, "Runtime.callFunctionOn", map[string]any{
			"functionDeclaration": `function() { this.click(); }`,
			"objectId":            objectID,
			"userGesture":         true,
		}, nil)
	}))
}

func resolveNodeObjectID(ctx context.Context, nodeID int64) (string, error) {
	var result json.RawMessage
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		return chromedp.FromContext(ctx).Target.Execute(ctx, "DOM.resolveNode", map[string]any{
			"backendNodeId": nodeID,
		}, &result)
	})); err != nil {
		return "", err
	}

	var resolved struct {
		Object struct {
			ObjectID string `json:"objectId"`
		} `json:"object"`
	}
	if err := json.Unmarshal(result, &resolved); err != nil {
		return "", err
	}
	if resolved.Object.ObjectID == "" {
		return "", fmt.Errorf("resolved node did not include object id")
	}

	return resolved.Object.ObjectID, nil
}

func resolveClickFallbackObjectID(ctx context.Context, nodeID int64) (string, error) {
	objectID, err := resolveNodeObjectID(ctx, nodeID)
	if err != nil {
		return "", err
	}

	var result json.RawMessage
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		return chromedp.FromContext(ctx).Target.Execute(ctx, "Runtime.callFunctionOn", map[string]any{
			"functionDeclaration": `function() {
				const interactiveSelector = 'a, button, input, select, textarea, summary, [role="button"], [role="link"], [role="menuitem"], [role="option"], [role="tab"], [tabindex]';

				function normalizeLabel(value) {
					return (value || '').replace(/\s+/g, ' ').trim().toLowerCase();
				}

				function isVisible(el) {
					if (!el || el.nodeType !== Node.ELEMENT_NODE)
						return false;
					const style = window.getComputedStyle(el);
					if (style.display === 'none' || style.visibility === 'hidden')
						return false;
					const rect = el.getBoundingClientRect();
					return rect.width > 0 && rect.height > 0;
				}

				function isInteractive(el) {
					return !!(el && el.matches && el.matches(interactiveSelector));
				}

				function labelFor(el) {
					if (!el)
						return '';
					if (el.getAttribute) {
						const ariaLabel = normalizeLabel(el.getAttribute('aria-label'));
						if (ariaLabel)
							return ariaLabel;
					}
					if ('value' in el) {
						const value = normalizeLabel(el.value);
						if (value)
							return value;
					}
					return normalizeLabel(el.textContent);
				}

				function chooseVisibleInteractive(root, targetLabel) {
					if (!root || !root.querySelectorAll)
						return null;

					const candidates = Array.from(root.querySelectorAll(interactiveSelector)).filter(candidate => {
						return isVisible(candidate) && isInteractive(candidate);
					});

					if (!targetLabel)
						return candidates[0] || null;

					for (const candidate of candidates) {
						if (labelFor(candidate) === targetLabel)
							return candidate;
					}
					return null;
				}

				const targetLabel = labelFor(this);

				if (isVisible(this) && isInteractive(this))
					return this;

				for (let ancestor = this.parentElement; ancestor; ancestor = ancestor.parentElement) {
					if (isVisible(ancestor) && isInteractive(ancestor))
						return ancestor;
				}

				const descendantMatch = chooseVisibleInteractive(this, targetLabel);
				if (descendantMatch)
					return descendantMatch;

				for (let container = this.parentElement; container; container = container.parentElement) {
					if (!isVisible(container))
						continue;
					const scopedMatch = chooseVisibleInteractive(container, targetLabel);
					if (scopedMatch)
						return scopedMatch;
				}

				const documentMatch = chooseVisibleInteractive(document, targetLabel);
				if (documentMatch)
					return documentMatch;

				return this;
			}`,
			"objectId":      objectID,
			"returnByValue": false,
		}, &result)
	})); err != nil {
		return "", err
	}

	var call struct {
		Result struct {
			ObjectID string `json:"objectId"`
		} `json:"result"`
	}
	if err := json.Unmarshal(result, &call); err != nil {
		return "", err
	}
	if call.Result.ObjectID == "" {
		return objectID, nil
	}

	return call.Result.ObjectID, nil
}

// getElementCenter returns the center coordinates and dimensions of an element using DOM.getBoxModel.
func getElementCenter(ctx context.Context, backendNodeID int64) (elementMetrics, error) {
	var result json.RawMessage
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		return chromedp.FromContext(ctx).Target.Execute(ctx, "DOM.getBoxModel", map[string]any{
			"backendNodeId": backendNodeID,
		}, &result)
	}))
	if err != nil {
		return elementMetrics{}, err
	}

	return parseElementMetrics(result)
}

func parseElementMetrics(result json.RawMessage) (elementMetrics, error) {
	var box struct {
		Model struct {
			Content []float64 `json:"content"`
		} `json:"model"`
	}
	if err := json.Unmarshal(result, &box); err != nil {
		return elementMetrics{}, err
	}

	if len(box.Model.Content) < 8 {
		return elementMetrics{}, fmt.Errorf("invalid box model: expected 8 coordinates")
	}

	minX, maxX := box.Model.Content[0], box.Model.Content[0]
	minY, maxY := box.Model.Content[1], box.Model.Content[1]
	for i := 0; i < len(box.Model.Content); i += 2 {
		x := box.Model.Content[i]
		y := box.Model.Content[i+1]
		if x < minX {
			minX = x
		}
		if x > maxX {
			maxX = x
		}
		if y < minY {
			minY = y
		}
		if y > maxY {
			maxY = y
		}
	}

	return elementMetrics{
		X:      (box.Model.Content[0] + box.Model.Content[2] + box.Model.Content[4] + box.Model.Content[6]) / 4,
		Y:      (box.Model.Content[1] + box.Model.Content[3] + box.Model.Content[5] + box.Model.Content[7]) / 4,
		Width:  maxX - minX,
		Height: maxY - minY,
	}, nil
}

func TypeByNodeID(ctx context.Context, nodeID int64, text string) error {
	return chromedp.Run(ctx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			return chromedp.FromContext(ctx).Target.Execute(ctx, "DOM.focus", map[string]any{"backendNodeId": nodeID}, nil)
		}),
		chromedp.KeyEvent(text),
	)
}

func HoverByNodeID(ctx context.Context, nodeID int64) error {
	metrics, err := getElementCenter(ctx, nodeID)
	if err != nil {
		return err
	}
	if metrics.Width <= 0 || metrics.Height <= 0 {
		return fmt.Errorf("cannot hover element with zero dimensions")
	}

	return chromedp.Run(ctx,
		// Scroll element into view first
		chromedp.ActionFunc(func(ctx context.Context) error {
			return chromedp.FromContext(ctx).Target.Execute(ctx, "DOM.scrollIntoViewIfNeeded", map[string]any{"backendNodeId": nodeID}, nil)
		}),
		// Move mouse to element center
		chromedp.ActionFunc(func(ctx context.Context) error {
			return chromedp.FromContext(ctx).Target.Execute(ctx, "Input.dispatchMouseEvent", map[string]any{
				"type": "mouseMoved",
				"x":    metrics.X, "y": metrics.Y,
			}, nil)
		}),
	)
}

func FillByNodeID(ctx context.Context, nodeID int64, value string) error {
	return chromedp.Run(ctx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			return chromedp.FromContext(ctx).Target.Execute(ctx, "DOM.focus", map[string]any{"backendNodeId": nodeID}, nil)
		}),
		chromedp.ActionFunc(func(ctx context.Context) error {
			objectID, err := resolveNodeObjectID(ctx, nodeID)
			if err != nil {
				return err
			}
			js := `function(v) { this.value = v; this.dispatchEvent(new Event('input', {bubbles: true})); this.dispatchEvent(new Event('change', {bubbles: true})); }`
			return chromedp.FromContext(ctx).Target.Execute(ctx, "Runtime.callFunctionOn", map[string]any{
				"functionDeclaration": js,
				"objectId":            objectID,
				"arguments":           []map[string]any{{"value": value}},
			}, nil)
		}),
	)
}

func SelectByNodeID(ctx context.Context, nodeID int64, value string) error {
	return chromedp.Run(ctx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			return chromedp.FromContext(ctx).Target.Execute(ctx, "DOM.focus", map[string]any{"backendNodeId": nodeID}, nil)
		}),
		chromedp.ActionFunc(func(ctx context.Context) error {
			objectID, err := resolveNodeObjectID(ctx, nodeID)
			if err != nil {
				return err
			}
			js := `function(v) { this.value = v; this.dispatchEvent(new Event('input', {bubbles: true})); this.dispatchEvent(new Event('change', {bubbles: true})); }`
			return chromedp.FromContext(ctx).Target.Execute(ctx, "Runtime.callFunctionOn", map[string]any{
				"functionDeclaration": js,
				"objectId":            objectID,
				"arguments":           []map[string]any{{"value": value}},
			}, nil)
		}),
	)
}

func ScrollByNodeID(ctx context.Context, nodeID int64) error {
	return chromedp.Run(ctx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			return chromedp.FromContext(ctx).Target.Execute(ctx, "DOM.scrollIntoViewIfNeeded", map[string]any{"backendNodeId": nodeID}, nil)
		}),
	)
}

func WaitForTitle(ctx context.Context, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		var title string
		if err := chromedp.Run(ctx, chromedp.Title(&title)); err != nil {
			return "", err
		}
		return title, nil
	}

	deadline := time.After(timeout)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline:
			var title string
			if err := chromedp.Run(ctx, chromedp.Title(&title)); err != nil {
				return "", err
			}
			return title, nil
		case <-ticker.C:
			var title string
			if err := chromedp.Run(ctx, chromedp.Title(&title)); err != nil {
				continue
			}
			if title != "" && title != "about:blank" {
				return title, nil
			}
		}
	}
}
