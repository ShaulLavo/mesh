package daemon

import "github.com/shaul/mesh/internal/storage"

func syncSleepInhibitor(update func(bool)) func([]storage.Session) {
	return func(sessions []storage.Session) {
		update(hasLiveWorker(sessions))
	}
}

func hasLiveWorker(sessions []storage.Session) bool {
	for _, session := range sessions {
		if session.State == storage.StateRunning || session.State == storage.StateDetached {
			return true
		}
	}
	return false
}
