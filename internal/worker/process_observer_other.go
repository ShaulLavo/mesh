//go:build !linux && !darwin

package worker

func defaultProcessObserver(int, int) processObservation {
	return processObservation{}
}
