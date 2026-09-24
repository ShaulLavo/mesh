//go:build !linux

package worker

func setHugePages(bool) {}

func hugePagesDisabled() bool { return false }
