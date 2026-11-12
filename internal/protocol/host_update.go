package protocol

func CloneHostUpdateSummary(value *HostUpdateSummary) *HostUpdateSummary {
	if value == nil {
		return nil
	}
	cloned := *value
	cloned.Target = cloneHostUpdateRelease(value.Target)
	cloned.Next = cloneHostUpdateRelease(value.Next)
	cloned.Head = cloneHostUpdateRelease(value.Head)
	cloned.Latest = cloneHostUpdateRelease(value.Latest)
	cloned.Lag = cloneHostUpdateValue(value.Lag)
	cloned.PendingSince = cloneHostUpdateValue(value.PendingSince)
	cloned.ObservedAt = cloneHostUpdateValue(value.ObservedAt)
	cloned.Last = cloneHostUpdateValue(value.Last)
	return &cloned
}

func cloneHostUpdateRelease(value *HostUpdateRelease) *HostUpdateRelease {
	cloned := cloneHostUpdateValue(value)
	if cloned != nil {
		cloned.PublishedAt = cloneHostUpdateValue(value.PublishedAt)
	}
	return cloned
}

func cloneHostUpdateValue[T any](value *T) *T {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
