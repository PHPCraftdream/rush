package gate

// ResumeCandidateSerial raises, never lowers, the step counter.
func (g *Gate) ResumeCandidateSerial(serial uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if serial > g.candidateSerial {
		g.candidateSerial = serial
	}
}
