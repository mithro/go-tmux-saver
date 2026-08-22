package collect

import (
	"strings"

	"github.com/mithro/go-tmux-saver/internal/snapshot"
)

func Unchanged(a, b *snapshot.Snapshot) bool {
	if len(a.Sessions) != len(b.Sessions) {
		return false
	}
	for i := range a.Sessions {
		sa, sb := a.Sessions[i], b.Sessions[i]
		if sa.Name != sb.Name || sa.ActiveWindow != sb.ActiveWindow || len(sa.Windows) != len(sb.Windows) {
			return false
		}
		for j := range sa.Windows {
			wa, wb := sa.Windows[j], sb.Windows[j]
			if wa.Index != wb.Index || wa.Name != wb.Name || wa.Layout != wb.Layout || wa.Active != wb.Active ||
				wa.Flags != wb.Flags || wa.AutomaticRename != wb.AutomaticRename || len(wa.Panes) != len(wb.Panes) {
				return false
			}
			for k := range wa.Panes {
				pa, pb := wa.Panes[k], wb.Panes[k]
				if pa.Index != pb.Index || pa.Cwd != pb.Cwd || pa.Active != pb.Active || !restoreEqual(pa.Restore, pb.Restore) {
					return false
				}
				if (strings.HasPrefix(pa.Title, "✳") || strings.HasPrefix(pb.Title, "✳")) && pa.Title != pb.Title {
					return false
				}
			}
		}
	}
	return true
}

func restoreEqual(x, y snapshot.Restore) bool {
	if x.Kind != y.Kind || x.ClaudeSession != y.ClaudeSession || len(x.Argv) != len(y.Argv) {
		return false
	}
	for i := range x.Argv {
		if x.Argv[i] != y.Argv[i] {
			return false
		}
	}
	return true
}
