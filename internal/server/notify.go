package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/hayone1/gitopsify/pkg/config"
)

// notifier posts finished runs to the configured webhooks.
type notifier struct {
	hooks    []config.ServerNotification
	external string
	client   *http.Client
	s        *Server
}

func (n *notifier) runFinished(r Run) {
	if n == nil || len(n.hooks) == 0 {
		return
	}
	event := "run." + r.Status
	for _, h := range n.hooks {
		if !slices.Contains(h.Events, event) || (len(h.Types) > 0 && !slices.Contains(h.Types, r.Type)) {
			continue
		}
		go n.post(h, event, r)
	}
}

// webhookURL resolves a notification's URL; the error never holds it (its
// path is often the secret).
func webhookURL(h config.ServerNotification) (string, error) {
	raw, err := h.URL.Resolve()
	if err != nil {
		return "", err
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", errors.New("the URL isn't an http(s) URL")
	}
	return raw, nil
}

func (n *notifier) post(h config.ServerNotification, event string, r Run) {
	defer func() {
		if v := recover(); v != nil {
			n.s.log.Error("notification panicked", "name", h.Name, "run", r.ID, "panic", fmt.Sprint(v))
		}
	}()
	target, err := webhookURL(h)
	if err != nil {
		n.s.log.Warn("notification", "name", h.Name, "error", err)
		return
	}
	body, err := n.payload(h.Format, event, r)
	if err != nil {
		return
	}
	var last error
	for attempt := range 3 {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt*attempt) * 2 * time.Second)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
		if err != nil {
			cancel()
			last = errors.New("the URL is invalid")
			break
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "gitopsify/"+n.s.version)
		resp, err := n.client.Do(req)
		cancel()
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // without the URL
		}
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 300 {
				return
			}
			err = fmt.Errorf("HTTP %d", resp.StatusCode)
			if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
				last = err
				break
			}
		}
		last = err
	}
	// The URL holds a secret: only its host is logged.
	host := ""
	if u, err := url.Parse(target); err == nil {
		host = u.Host
	}
	n.s.log.Warn("notification failed", "name", h.Name, "host", host, "run", r.ID, "error", last)
}

func (n *notifier) payload(format, event string, r Run) ([]byte, error) {
	link := strings.TrimSuffix(n.external, "/") + "/runs/" + r.ID
	what := runTypeLabel(r.Type)
	where := firstNonEmpty(r.Cluster, r.Tool)
	text := fmt.Sprintf("gitopsify %s %s %s on %s (by %s)", what, firstNonEmpty(r.Name, r.ID), r.Status, where, r.CreatedBy)
	if r.Error != "" {
		text += ": " + r.Error
	}
	switch format {
	case "slack":
		icon := map[string]string{"succeeded": ":white_check_mark:", "failed": ":x:", "canceled": ":no_entry_sign:"}[r.Status]
		return json.Marshal(map[string]any{"text": fmt.Sprintf("%s %s <%s|view the run>", icon, text, link)})
	case "teams":
		return json.Marshal(map[string]any{
			"type": "message",
			"attachments": []any{map[string]any{
				"contentType": "application/vnd.microsoft.card.adaptive",
				"content": map[string]any{
					"type": "AdaptiveCard", "version": "1.4", "$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
					"body":    []any{map[string]any{"type": "TextBlock", "text": text, "wrap": true}},
					"actions": []any{map[string]any{"type": "Action.OpenUrl", "title": "View the run", "url": link}},
				},
			}},
		})
	}
	r.Spec = nil
	return json.Marshal(map[string]any{"event": event, "run": r, "url": link})
}

func runTypeLabel(t string) string {
	switch t {
	case RunMigrate:
		return "migration"
	}
	return t
}
