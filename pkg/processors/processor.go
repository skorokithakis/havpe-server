package processors

// TranscriptRequest is the input passed through the processor chain.
// Processors may read and modify it before passing it along.
type TranscriptRequest struct {
	Transcript string
}

// TranscriptResponse is the output of a processor. It carries an optional
// response text for TTS and controls whether the chain continues.
type TranscriptResponse struct {
	// ResponseText is synthesized to speech and played on the device.
	// Empty means play a confirmation tone instead.
	ResponseText string
	// StopProcessing tells the pipeline to stop iterating the processor chain.
	// When false, subsequent processors still see the (possibly modified) request.
	StopProcessing bool
}

// TranscriptProcessor processes a voice transcript. Implementations are called
// in order. Each processor receives the request and the response accumulated so
// far, and may modify either. Set resp.StopProcessing to end the chain early.
type TranscriptProcessor interface {
	Process(req *TranscriptRequest, resp *TranscriptResponse) error
}
