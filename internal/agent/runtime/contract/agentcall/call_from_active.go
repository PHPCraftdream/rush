package agentcall

// CallFromActive copies an active-call template while clearing call-kind and notice state.
func CallFromActive(active SessionAgentCall) SessionAgentCall {
	call := active
	call.IsDrain = false
	call.DrainTurnCommitted = false
	call.AutoResumed = false
	call.BackgroundJobNotice = false
	call.NoticeKind = ""
	return call
}
