package controller

import "time"

// escalationMachine owns the blocked-output escalation ladder: the controller
// wants discharge and the controlled slot is armed, but the device reports no
// battery contribution. It counts consecutive blocked cycles, remembers the
// last output-enable attempt, gates the restart-disabled warning to once per
// blocked regime, and rate-limits the opt-in nuclear restart. Every path
// that leaves the regime goes through reset, so no exit can forget a field.
type escalationMachine struct {
	blockedCycles        int
	lastEnableAttemptAt  time.Time
	loggedRestartBlocked bool
	lastRestartAt        time.Time
}

// reset clears the blocked-output evidence. Called on every path that leaves
// the precondition-failure regime: healthy battery contribution, slot
// changes, deliberate idle, fallback, or a completed restart.
func (e *escalationMachine) reset() {
	e.blockedCycles = 0
	e.lastEnableAttemptAt = time.Time{}
	e.loggedRestartBlocked = false
}

// noteBlockedCycle counts one more cycle with the slot armed but no battery
// contribution.
func (e *escalationMachine) noteBlockedCycle() {
	e.blockedCycles++
}

// recordEnableAttempt records that an output-enable remediation was published
// at at. Nuclear restart requires one attempt first.
func (e *escalationMachine) recordEnableAttempt(at time.Time) {
	e.lastEnableAttemptAt = at
}

// enableAttempted reports whether an output-enable remediation has been
// attempted during the current blocked regime.
func (e *escalationMachine) enableAttempted() bool {
	return !e.lastEnableAttemptAt.IsZero()
}

// shouldWarnRestartDisabled marks the restart-disabled warning as logged for
// this regime and reports whether the caller should emit it.
func (e *escalationMachine) shouldWarnRestartDisabled() bool {
	if e.loggedRestartBlocked {
		return false
	}
	e.loggedRestartBlocked = true
	return true
}

// restartRateLimited reports whether a nuclear restart was commanded more
// recently than minInterval.
func (e *escalationMachine) restartRateLimited(now time.Time, minInterval time.Duration) bool {
	return minInterval > 0 && !e.lastRestartAt.IsZero() && now.Sub(e.lastRestartAt) < minInterval
}

// recordNuclearRestart records that a device restart was commanded at at and
// clears the blocked-output evidence that triggered it.
func (e *escalationMachine) recordNuclearRestart(at time.Time) {
	e.lastRestartAt = at
	e.reset()
}
