package protocol

import "github.com/shaul/mesh/internal/recovery"

// LeanRecoveryInfo keeps recognition and activity facts while leaving screen
// text and execution-only details to an explicit recovery or inspection read.
func LeanRecoveryInfo(info *SessionInfo, record recovery.Record) {
	record.Lines = nil
	record.Command = nil
	record.AgentResume = nil
	if record.Agent != nil {
		agent := *record.Agent
		agent.Options = nil
		record.Agent = &agent
	}
	info.Recovery = &record
	info.RecoveryDetailsOmitted = true
}
