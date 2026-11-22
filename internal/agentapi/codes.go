package agentapi

type Code string

const (
	UsageInvalid             Code = "usage.invalid"
	HostUnknown              Code = "host.unknown"
	HostAmbiguous            Code = "host.ambiguous"
	HostUnreachable          Code = "host.unreachable"
	HostIdentityMismatch     Code = "host.identity_mismatch"
	HostAuthenticationFailed Code = "host.authentication_failed"
	HostUninitialized        Code = "host.uninitialized"
	ProtocolUnsupported      Code = "protocol.unsupported"
	StateUnavailable         Code = "state.unavailable"
	SessionNotFound          Code = "session.not_found"
	ProfileInvalid           Code = "profile.invalid"
	ProfileConflict          Code = "profile.conflict"
	ProfileRegressed         Code = "profile.regressed"
	FleetSignerUntrusted     Code = "fleet.signer_untrusted"
	FleetDeliveryRejected    Code = "fleet.delivery_rejected"
	FleetRevisionConflict    Code = "fleet.revision_conflict"
	FleetStale               Code = "fleet.stale"
	SkillConflict            Code = "skill.conflict"
	SkillUnsafePath          Code = "skill.unsafe_path"
	RequestKeyConflict       Code = "request.key_conflict"
	RequestInProgress        Code = "request.in_progress"
	RequestOutcomeUnknown    Code = "request.outcome_unknown"
	RequestNotFound          Code = "request.not_found"
	AccessDenied             Code = "access.denied"
	ResourceLimit            Code = "resource.limit"
	LogsCursorExpired        Code = "logs.cursor_expired"
	LogsCursorInvalid        Code = "logs.cursor_invalid"
	ResultExpired            Code = "result.expired"
	SessionInstanceMismatch  Code = "session.instance_mismatch"
	InputOutcomeUnknown      Code = "input.outcome_unknown"
	InputKeyConflict         Code = "input.key_conflict"
	InputOffsetMismatch      Code = "input.offset_mismatch"
	InputClosed              Code = "input.closed"
	InputBackpressure        Code = "input.backpressure"
	InternalError            Code = "internal.error"
	FileNotFound             Code = "file.not_found"
	FileNotRegular           Code = "file.not_regular"
	FileChanged              Code = "file.changed"
	FileCursorInvalid        Code = "file.cursor_invalid"
	FileConflict             Code = "file.conflict"
	FilePatchMismatch        Code = "file.patch_mismatch"
	FilePermissionDenied     Code = "file.permission_denied"
	FileTargetMismatch       Code = "file.target_mismatch"
	ContextNotFound          Code = "context.not_found"
	ContextClosed            Code = "context.closed"
	ContextConflict          Code = "context.conflict"
)

type ErrorClass struct {
	Exit      int
	Retryable bool
	Reserved  bool
}

var Codes = map[Code]ErrorClass{
	UsageInvalid: {Exit: 2}, HostUnknown: {Exit: 1}, HostAmbiguous: {Exit: 1},
	HostUnreachable: {Exit: 1, Retryable: true}, HostIdentityMismatch: {Exit: 1},
	HostAuthenticationFailed: {Exit: 1, Retryable: true}, HostUninitialized: {Exit: 1},
	ProtocolUnsupported: {Exit: 1}, StateUnavailable: {Exit: 1, Retryable: true}, SessionNotFound: {Exit: 1},
	ProfileInvalid: {Exit: 2}, ProfileConflict: {Exit: 1}, ProfileRegressed: {Exit: 3},
	FleetSignerUntrusted: {Exit: 1, Retryable: true}, FleetDeliveryRejected: {Exit: 1, Retryable: true},
	FleetRevisionConflict: {Exit: 1}, FleetStale: {Exit: 1}, SkillConflict: {Exit: 1}, SkillUnsafePath: {Exit: 2}, InternalError: {Exit: 1},
	RequestKeyConflict: {Exit: 1}, RequestInProgress: {Exit: 1, Retryable: true}, RequestNotFound: {Exit: 1},
	RequestOutcomeUnknown: {Exit: 1}, LogsCursorExpired: {Exit: 1}, LogsCursorInvalid: {Exit: 1}, SessionInstanceMismatch: {Exit: 1},
	InputOutcomeUnknown: {Exit: 1}, InputKeyConflict: {Exit: 1}, InputOffsetMismatch: {Exit: 1},
	InputClosed: {Exit: 1}, InputBackpressure: {Exit: 1, Retryable: true},
	ResultExpired: {Exit: 1}, AccessDenied: {Exit: 1}, ResourceLimit: {Exit: 1},
	FileNotFound: {Exit: 1}, FileNotRegular: {Exit: 1}, FileChanged: {Exit: 1}, FileCursorInvalid: {Exit: 1},
	FileConflict: {Exit: 1}, FilePatchMismatch: {Exit: 1}, FilePermissionDenied: {Exit: 1}, FileTargetMismatch: {Exit: 1},
	ContextNotFound: {Exit: 1}, ContextClosed: {Exit: 1}, ContextConflict: {Exit: 1},
}
