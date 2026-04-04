package processors

// TranscriptRequest is the input passed through the processor chain.
type TranscriptRequest struct {
	Transcript string
}

// TranscriptResponse is the output of a processor.
type TranscriptResponse struct {
	// ResponseText is synthesized to speech and played on the device.
	// Empty means play a confirmation tone instead.
	ResponseText string
	// StopProcessing tells the pipeline to stop iterating the processor chain.
	// When false, subsequent processors still run and may replace this response.
	StopProcessing bool
}

// TranscriptProcessor processes a voice transcript. Implementations are called
// in order. Each processor receives the request and returns a response.
// The pipeline uses the last non-nil response. Set StopProcessing to end the
// chain early. Return (nil, nil) to pass through without contributing a response.
type TranscriptProcessor interface {
	Process(req *TranscriptRequest) (*TranscriptResponse, error)
}
