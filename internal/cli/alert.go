package cli

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/mithro/go-tmux-saver/internal/config"
	"github.com/mithro/go-tmux-saver/internal/mail"
)

// alertBody renders the mail body for a failure/recovery alert: the same
// text `status` prints (last good save, timer state, data dir, and the last
// n events) — RunStatus already includes the events section, so nothing
// further needs appending.
func alertBody(dataDir string, cfg config.Config, n int) string {
	var buf bytes.Buffer
	RunStatus(&buf, dataDir, cfg, false, false, n, time.Now())
	return buf.String()
}

func init() {
	register(command{"alert", "send a rate-limited sendmail alert for a failed or recovered unit", func(args []string, stdout, stderr io.Writer) int {
		fs := flag.NewFlagSet("alert", flag.ContinueOnError)
		unit := fs.String("unit", "", "systemd unit name that failed/recovered (required)")
		recovered := fs.Bool("recovered", false, "send a recovery mail instead of a failure alert")
		socket := fs.String("socket", "", "override config socket")
		dataDir := fs.String("data-dir", "", "override config data dir")
		cfgPath := fs.String("config", config.Path(), "config file")
		fs.SetOutput(stderr)
		if err := fs.Parse(args); err != nil {
			return 2
		}
		if *unit == "" {
			fmt.Fprintln(stderr, "alert: --unit is required")
			return 2
		}

		cfg, store, msg, code := commonSetup(*cfgPath, *socket, *dataDir)
		if code != 0 {
			fmt.Fprintln(stderr, msg)
			return code
		}

		rl := mail.RateLimiter{Dir: store.Dir}
		var send bool
		if *recovered {
			send = rl.Clear(*unit)
		} else {
			send = rl.ShouldSend(*unit, time.Now())
		}
		if !send {
			fmt.Fprintln(stdout, "rate-limited: no mail sent")
			return 0
		}

		host, err := os.Hostname()
		if err != nil {
			host = "unknown-host"
		}
		verb := "failed"
		if *recovered {
			verb = "recovered"
		}
		subject := fmt.Sprintf("[go-tmux-saver] %s: %s %s", host, *unit, verb)
		body := alertBody(store.Dir, cfg, 20)

		if err := mail.Send(mail.Sendmail, cfg.MailTo, subject, body); err != nil {
			fmt.Fprintln(stderr, "alert: sendmail:", err)
			return 1
		}
		fmt.Fprintln(stdout, "sent:", subject)
		return 0
	}})
}
