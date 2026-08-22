// Package procs reads the process table once and resolves pane processes.
package procs

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Proc struct {
	PID, PPID int
	Comm      string
	Cmdline   []string
	StartTime string // /proc/<pid>/stat field 22, opaque string
}

type Table struct {
	byPID    map[int]Proc
	children map[int][]int
}

// Scan reads every numeric directory under procRoot ("/proc" in production).
func Scan(procRoot string) (*Table, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
	}
	t := &Table{byPID: map[int]Proc{}, children: map[int][]int{}}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "stat"))
		if err != nil {
			continue // process exited mid-scan
		}
		p, ok := parseStat(pid, stat)
		if !ok {
			continue
		}
		if cl, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "cmdline")); err == nil {
			for _, part := range bytes.Split(bytes.TrimRight(cl, "\x00"), []byte{0}) {
				p.Cmdline = append(p.Cmdline, string(part))
			}
		}
		t.byPID[pid] = p
		t.children[p.PPID] = append(t.children[p.PPID], pid)
	}
	return t, nil
}

// parseStat handles comm containing spaces/parens by splitting on the LAST ')'.
func parseStat(pid int, stat []byte) (Proc, bool) {
	s := string(stat)
	lp, rp := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if lp < 0 || rp < 0 || rp < lp {
		return Proc{}, false
	}
	rest := strings.Fields(s[rp+1:]) // rest[0]=state rest[1]=ppid … rest[19]=starttime
	if len(rest) < 20 {
		return Proc{}, false
	}
	ppid, err := strconv.Atoi(rest[1])
	if err != nil {
		return Proc{}, false
	}
	return Proc{PID: pid, PPID: ppid, Comm: s[lp+1 : rp], StartTime: rest[19]}, true
}

func (t *Table) Get(pid int) (Proc, bool) { p, ok := t.byPID[pid]; return p, ok }

// Subtree returns pid and all descendants, breadth-first (shallowest first),
// children in ascending pid order for determinism.
func (t *Table) Subtree(pid int) []int {
	out := []int{pid}
	for i := 0; i < len(out); i++ {
		kids := append([]int(nil), t.children[out[i]]...)
		sortInts(kids)
		out = append(out, kids...)
	}
	return out
}

func sortInts(a []int) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j-1] > a[j]; j-- {
			a[j-1], a[j] = a[j], a[j-1]
		}
	}
}
