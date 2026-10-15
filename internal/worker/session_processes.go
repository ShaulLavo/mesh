package worker

import "errors"

// ErrCommandGone means a session's command and every process in its kernel
// session have exited, so there is nothing left to list.
var ErrCommandGone = errors.New("command process is gone")
