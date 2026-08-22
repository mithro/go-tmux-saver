package snapshot

// IsDegenerate reports whether a new save collapsed enough, relative to a
// rich last save, to look like an accidental clobber (e.g. a 1-pane save
// right after boot). Only fires when last had >= minPanes.
func IsDegenerate(newPanes, lastPanes, minPanes, divisor int) bool {
	if lastPanes < minPanes {
		return false
	}
	return newPanes*divisor <= lastPanes
}
