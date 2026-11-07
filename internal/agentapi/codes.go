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
	InternalError            Code = "internal.error"
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
	RequestOutcomeUnknown: {Exit: 1}, "logs.cursor_expired": {Exit: 1, Reserved: true},
	"result.expired": {Exit: 1, Reserved: true}, AccessDenied: {Exit: 1}, ResourceLimit: {Exit: 1},
}
