package mcpnative

// DropNotifications discards this request's notifications that are queued
// but not yet written. A stream that ends because its authorization was
// retired calls it, so nothing queued before the retirement reaches stdout.
func (r *StdioResponder) DropNotifications() {
	r.engine.mu.Lock()
	defer r.engine.mu.Unlock()
	r.engine.dropOwnedFramesLocked(r.entry.key)
	r.engine.removeProgressLocked(r.entry.key)
}
