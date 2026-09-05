//go:build !linux && !darwin

package worker

func readAncestorProcess(int) (ancestorProcess, bool) {
	return ancestorProcess{}, false
}
