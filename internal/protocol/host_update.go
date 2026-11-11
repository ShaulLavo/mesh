package protocol

func CloneHostUpdateSummary(value *HostUpdateSummary) *HostUpdateSummary {
	if value == nil {
		return nil
	}
	cloned := *value
	cloned.Target = cloneHostUpdateValue(value.Target)
	cloned.Next = cloneHostUpdateValue(value.Next)
	cloned.Head = cloneHostUpdateValue(value.Head)
	cloned.Latest = cloneHostUpdateValue(value.Latest)
	cloned.Lag = cloneHostUpdateValue(value.Lag)
	cloned.PendingSince = cloneHostUpdateValue(value.PendingSince)
	cloned.ObservedAt = cloneHostUpdateValue(value.ObservedAt)
	cloned.Last = cloneHostUpdateValue(value.Last)
	return &cloned
}

func cloneHostUpdateValue[T any](value *T) *T {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
