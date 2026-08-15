Four things worth your attention
1. The two-429 test is the heart of it (openai_test.go:139). A throttling 429 comes back Retryable: true; a 429 with type: insufficient_quota — billing exhausted — comes back Retryable: false. Same status code, opposite handling, and only the response body distinguishes them. That's the concrete proof of the DECISIONS.md claim about why Retryable is stored rather than derived. When an interviewer asks, this is the example.

2. Timeouts go through context, not http.Client.Timeout (openai.go:99). Wrapping the caller's ctx means the sooner of the two deadlines wins automatically. Client.Timeout would silently override a shorter caller deadline — and Phase 6 requires that retries never exceed the client's deadline, which only works if the caller's ctx is authoritative. There's a test asserting a 20ms caller deadline beats the 2s instance timeout.

3. I tuned the transport, and it's not incidental (http.go:36). Go's default allows two idle connections per host. Under any real load the gateway would spend its time in TCP and TLS handshakes, which lands directly on your sub-10ms overhead target. Raised to 100.

4. Bodies are bounded at 10 MiB (http.go:17). Plain io.ReadAll on an upstream response is unbounded allocation driven by a third party. "The gateway must never be the reason a request fails" includes not OOMing because a provider misbehaved.