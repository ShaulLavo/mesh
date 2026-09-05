//go:build !linux && !darwin

package worker

func platformBootID() string { return "" }
