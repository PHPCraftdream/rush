package sessionprompt

// NoticeKindSupervision tags a supervision tick's persisted message
// (Message.NoticeKind) -- both the ordinary check-in and the terminal
// "paused" notice use it, so agent_prompt.go's history filter treats them
// uniformly (only the newest one is ever shown to the model).
const NoticeKindSupervision = "supervision"
