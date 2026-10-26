package sync

import "context"

// FailIndexFollowsForTest makes every read of whether an account's index
// still follows fail with err, as a reader the database refused would. It is
// set before Run.
func FailIndexFollowsForTest(m *Manager, err error) {
	m.indexFollows = func(context.Context, string) (bool, error) { return false, err }
}
