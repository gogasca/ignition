package store

// validObservedTransition prevents a stale controller replica from moving a
// sandbox backwards after a newer observation or termination request.
func validObservedTransition(from, to string) bool {
	if from == "FINISHED" || from == "FAILED" {
		return false
	}
	if to == "FAILED" || to == "FINISHED" {
		return true
	}
	if from == "TERMINATING" {
		return false
	}
	rank := map[string]int{"CREATING": 1, "SCHEDULED": 2, "STARTED": 3, "READY": 4}
	return rank[to] > rank[from]
}

func validProcessTransition(from, to string) bool {
	if from == "EXITED" || from == "FAILED" || from == "CANCELLING" {
		return false
	}
	if to == "EXITED" || to == "FAILED" {
		return true
	}
	rank := map[string]int{"CREATING": 1, "STARTING": 2, "RUNNING": 3}
	return rank[to] > rank[from]
}
