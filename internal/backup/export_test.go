package backup

// ParseSnapshotsForTest exposes the restic listing parser to the round-trip
// test, which lives in backup_test and so cannot reach it directly.
//
// Exported here rather than in the package proper because nothing outside needs
// it: callers get snapshots from Snapshots, which runs the listing and parses
// it in one step. What the test needs is to parse output it captured itself, so
// that a parser broken against real restic output fails as a parser bug rather
// than as a task that would not run.
func ParseSnapshotsForTest(output string) ([]Snapshot, error) {
	return parseSnapshots(output)
}
