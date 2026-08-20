package app

type networkRefreshState struct {
	lastSuccessfulFingerprint string
	lastError                 string
	pending                   bool
}

func newNetworkRefreshState(fingerprint string) *networkRefreshState {
	return &networkRefreshState{lastSuccessfulFingerprint: fingerprint}
}

func (state *networkRefreshState) shouldAttempt(fingerprint string, wokeFromSleep bool) bool {
	return state.pending || wokeFromSleep || fingerprint != state.lastSuccessfulFingerprint
}

// failed keeps refresh pending even when the recovered topology later has the
// exact same fingerprint as the last successful topology. It returns whether
// the error changed and should be logged.
func (state *networkRefreshState) failed(err error) bool {
	state.pending = true
	message := err.Error()
	changed := message != state.lastError
	state.lastError = message
	return changed
}

func (state *networkRefreshState) succeeded(fingerprint string) {
	state.lastSuccessfulFingerprint = fingerprint
	state.lastError = ""
	state.pending = false
}

func (state *networkRefreshState) reset(fingerprint string) {
	state.succeeded(fingerprint)
}
