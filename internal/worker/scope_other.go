//go:build !linux

package worker

// IsolateSession is a no-op where systemd scopes do not exist.
func IsolateSession(string) {}
