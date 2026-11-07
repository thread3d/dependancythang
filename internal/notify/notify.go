// Package notify delivers the result of an unattended run.
//
// A scheduled run happens when nobody is watching, so its outcome has to travel
// somewhere (§3 row 14). Every notifier is best effort: a failed notification
// must never fail the run that produced it.
package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Message is one notification.
type Message struct {
	Title  string    `json:"title"`
	Body   string    `json:"body"`
	Repo   string    `json:"repo,omitempty"`
	RunID  string    `json:"run_id,omitempty"`
	Status string    `json:"status,omitempty"`
	At     time.Time `json:"at"`
}

// Notifier delivers a message.
type Notifier interface {
	Name() string
	Send(ctx context.Context, msg Message) error
}

// Multi fans out to several notifiers, collecting errors without stopping: one
// broken channel must not silence the others.
type Multi struct{ Notifiers []Notifier }

// Name implements Notifier.
func (m Multi) Name() string { return "multi" }

// Send implements Notifier.
func (m Multi) Send(ctx context.Context, msg Message) error {
	var failures []string
	for _, n := range m.Notifiers {
		if n == nil {
			continue
		}
		if err := n.Send(ctx, msg); err != nil {
			failures = append(failures, n.Name()+": "+err.Error())
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("notification failures: %s", strings.Join(failures, "; "))
	}
	return nil
}

// Desktop posts a freedesktop notification through notify-send. It is silently
// unavailable on machines without a session bus.
type Desktop struct{}

// Name implements Notifier.
func (Desktop) Name() string { return "desktop" }

// Send implements Notifier.
func (Desktop) Send(ctx context.Context, msg Message) error {
	path, err := exec.LookPath("notify-send")
	if err != nil {
		return fmt.Errorf("notify-send not found")
	}
	cmd := exec.CommandContext(ctx, path, "--app-name=depmcp", msg.Title, msg.Body)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("notify-send: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Ntfy posts to an ntfy topic, which is the cheapest way to reach a phone when a
// scheduled run finds something (§3 row 14).
type Ntfy struct {
	Server string
	Topic  string
	// HTTP is replaced in tests.
	HTTP *http.Client
}

// Name implements Notifier.
func (n *Ntfy) Name() string { return "ntfy" }

// Send implements Notifier.
func (n *Ntfy) Send(ctx context.Context, msg Message) error {
	if strings.TrimSpace(n.Topic) == "" {
		return fmt.Errorf("ntfy topic is empty")
	}
	server := strings.TrimSuffix(strings.TrimSpace(n.Server), "/")
	if server == "" {
		server = "https://ntfy.sh"
	}
	endpoint := server + "/" + url.PathEscape(n.Topic)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(msg.Body))
	if err != nil {
		return err
	}
	req.Header.Set("Title", sanitizeHeader(msg.Title))
	if msg.Status != "" {
		req.Header.Set("Tags", sanitizeHeader(msg.Status))
	}
	client := n.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("ntfy %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy %s: unexpected status %s", endpoint, resp.Status)
	}
	return nil
}

// sanitizeHeader strips characters that would break an HTTP header value.
func sanitizeHeader(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return ' '
		}
		return r
	}, s)
}

// File appends notifications as JSON lines, which is what an unattended run on a
// headless box needs: a durable record instead of a transient popup.
type File struct {
	Dir string
}

// Name implements Notifier.
func (f File) Name() string { return "file" }

// Send implements Notifier.
func (f File) Send(_ context.Context, msg Message) error {
	if f.Dir == "" {
		return fmt.Errorf("no notification directory configured")
	}
	if err := os.MkdirAll(f.Dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", f.Dir, err)
	}
	if msg.At.IsZero() {
		msg.At = time.Now().UTC()
	}
	line, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	path := filepath.Join(f.Dir, "notifications.jsonl")
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer fh.Close()
	if _, err := fh.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// Noop discards messages; used when notification is disabled.
type Noop struct{}

// Name implements Notifier.
func (Noop) Name() string { return "none" }

// Send implements Notifier.
func (Noop) Send(context.Context, Message) error { return nil }
