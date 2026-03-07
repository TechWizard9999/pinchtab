package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

func TestAXNodeResolution_Regression115(t *testing.T) {
	html := `<!DOCTYPE html>
<html>
<head>
<style>
  .dropdown-menu { display: none; }
  .dropdown.show .dropdown-menu { display: block; }
  nav { padding: 10px; background: #eee; }
  .visible-button { margin-top: 50px; padding: 10px; }
  body { font-family: sans-serif; }
</style>
</head>
<body>
  <nav id="nav">
    <div class="dropdown">
      <a href="#" class="dropdown-toggle" id="visible-toggle" role="button">Login</a>
      <ul class="dropdown-menu">
        <li id="hidden-wrapper"><a class="dropdown-item" href="#" id="hidden-login" role="menuitem">Login</a></li>
      </ul>
    </div>
  </nav>
  <main>
    <button type="submit" id="submit-login" class="visible-button">Login</button>
  </main>
  <script>
    window.clicks = [];
    document.addEventListener('click', e => {
      let id = e.target.id || e.target.tagName;
      if (e.target.closest('#nav') && !e.target.id) id = 'nav';
      window.clicks.push({id: id, x: e.clientX, y: e.clientY});
    });
  </script>
</body>
</html>`

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(html))
	}))
	defer ts.Close()

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", "new"),
		chromedp.Flag("disable-gpu", true),
	)
	allocCtx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()

	ctx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()

	ctx, cancel = context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	if err := NavigatePage(ctx, ts.URL); err != nil {
		t.Fatalf("Failed to navigate: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	var hiddenBackendNodeID int64
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		var docResult json.RawMessage
		chromedp.FromContext(c).Target.Execute(c, "DOM.getDocument", map[string]any{"depth": 0}, &docResult)
		var doc struct{ Root struct{ NodeID int64 `json:"nodeId"` } `json:"root"` }
		json.Unmarshal(docResult, &doc)

		var qResult json.RawMessage
		chromedp.FromContext(c).Target.Execute(c, "DOM.querySelector", map[string]any{
			"nodeId":   doc.Root.NodeID,
			"selector": "#hidden-wrapper a",
		}, &qResult)
		var qr struct{ NodeID int64 `json:"nodeId"` }
		json.Unmarshal(qResult, &qr)

		var descResult json.RawMessage
		chromedp.FromContext(c).Target.Execute(c, "DOM.describeNode", map[string]any{"nodeId": qr.NodeID}, &descResult)
		var desc struct{ Node struct{ BackendNodeID int64 `json:"backendNodeId"` } `json:"node"` }
		json.Unmarshal(descResult, &desc)
		hiddenBackendNodeID = desc.Node.BackendNodeID
		return nil
	}))
	if err != nil {
		t.Fatalf("Failed to get hidden element backendNodeId: %v", err)
	}

	// Because of the bug, pinchtab thinks the node ID is the hidden menu item.
	// We want to test clicking it.
	err = ClickByNodeID(ctx, hiddenBackendNodeID)
	if err != nil {
		t.Fatalf("ClickByNodeID failed: %v", err)
	}

	var clicks []map[string]any
	chromedp.Run(ctx, chromedp.Evaluate(`window.clicks`, &clicks))

	if len(clicks) == 0 {
		t.Fatalf("Expected click to be registered")
	}

	clickedID := clicks[len(clicks)-1]["id"].(string)

	// Since the user deleted ResolveActionableNode, ClickByNodeID will call fallbackJSClick
	// directly on the hidden-login element. It will NOT click the submit-login element.
	// But wait, the issue is that both e13 and e34 point to the hidden element.
	// The ultimate goal is that clicking e34 should resolve to the submit button.
	// Without ResolveActionableNode evaluating the whole DOM for a visible element with the same text,
	// or without ResolveActionableNode walking up/down the tree, we're just clicking the hidden element.

	if clickedID == "hidden-login" {
		t.Errorf("Click incorrectly landed directly on the hidden element (hidden-login). Expected it to resolve to a visible element like 'submit-login' or 'visible-toggle' first.")
	}
}
