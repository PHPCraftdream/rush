package session

// Question-stop finish framing shared by agent producers and session recovery.
const (
	AwaitingAnswerStoppedTitle = "Stopped: agent asked a question and is awaiting an answer"
	AwaitingQuestionPrefix     = "QUESTION: "
	AwaitingGuidanceMarker     = "\n\nThis is not a crash"
)
