package snapshot

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Event struct {
	Time                              time.Time
	Outcome                           string
	Panes, Windows, Sessions, Clients int
	DurationMS                        int64
	File, Detail                      string
}

const eventsFile = "events.log"
const freshFile = "fresh"

func sanitizeField(s string) string {
	return strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(s)
}

func AppendEvent(dir string, e Event) error {
	f, err := os.OpenFile(filepath.Join(dir, eventsFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s\t%s\tpanes=%d\twindows=%d\tsessions=%d\tclients=%d\tduration_ms=%d\t%s\t%s\n",
		e.Time.UTC().Format(time.RFC3339), e.Outcome, e.Panes, e.Windows, e.Sessions, e.Clients, e.DurationMS, sanitizeField(e.File), sanitizeField(e.Detail))
	return err
}

func TailEvents(dir string, n int) ([]Event, error) {
	f, err := os.Open(filepath.Join(dir, eventsFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var all []Event
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if e, ok := parseEvent(sc.Text()); ok {
			all = append(all, e)
		}
	}
	if len(all) > n {
		all = all[len(all)-n:]
	}
	return all, sc.Err()
}

func parseEvent(line string) (Event, bool) {
	f := strings.Split(line, "\t")
	if len(f) < 9 {
		return Event{}, false
	}
	ts, err := time.Parse(time.RFC3339, f[0])
	if err != nil {
		return Event{}, false
	}
	kv := func(s string) int { i, _ := strconv.Atoi(s[strings.IndexByte(s, '=')+1:]); return i }
	return Event{Time: ts, Outcome: f[1], Panes: kv(f[2]), Windows: kv(f[3]), Sessions: kv(f[4]),
		Clients: kv(f[5]), DurationMS: int64(kv(f[6])), File: f[7], Detail: f[8]}, true
}

func TouchFresh(dir string) error {
	p := filepath.Join(dir, freshFile)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	f.Close()
	now := time.Now()
	return os.Chtimes(p, now, now)
}

func LastGood(dir string) (time.Time, bool, error) {
	fi, err := os.Stat(filepath.Join(dir, freshFile))
	if err != nil {
		if os.IsNotExist(err) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, err
	}
	return fi.ModTime(), true, nil
}
