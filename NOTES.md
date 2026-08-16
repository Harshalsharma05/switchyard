Four things worth your attention
1. The two-429 test is the heart of it (openai_test.go:139). A throttling 429 comes back Retryable: true; a 429 with type: insufficient_quota — billing exhausted — comes back Retryable: false. Same status code, opposite handling, and only the response body distinguishes them. That's the concrete proof of the DECISIONS.md claim about why Retryable is stored rather than derived. When an interviewer asks, this is the example.

2. Timeouts go through context, not http.Client.Timeout (openai.go:99). Wrapping the caller's ctx means the sooner of the two deadlines wins automatically. Client.Timeout would silently override a shorter caller deadline — and Phase 6 requires that retries never exceed the client's deadline, which only works if the caller's ctx is authoritative. There's a test asserting a 20ms caller deadline beats the 2s instance timeout.

3. I tuned the transport, and it's not incidental (http.go:36). Go's default allows two idle connections per host. Under any real load the gateway would spend its time in TCP and TLS handshakes, which lands directly on your sub-10ms overhead target. Raised to 100.

4. Bodies are bounded at 10 MiB (http.go:17). Plain io.ReadAll on an upstream response is unbounded allocation driven by a third party. "The gateway must never be the reason a request fails" includes not OOMing because a provider misbehaved.


What _test.go files are
It's a Go convention, not a project choice: any file ending in _test.go is excluded from the normal build and only compiled when you run go test. The go test tool automatically discovers every _test.go file in a package and runs any function shaped like func TestXxx(t *testing.T).

They live next to the code they test, in the same package — openai.go and openai_test.go are both package provider. That's different from Python/pytest, where tests usually live in a separate tests/ directory. In Go, a test file can see unexported identifiers (lowercase names like truncateMessage, validateConfig) because it's technically part of the same package. That's why http_test.go can call parseRetryAfter directly even though it's never exported.

Are they "needed" to run the gateway?
No. go build and go run ignore _test.go files completely — they will never end up in your compiled gw.exe, and deleting every test file right now would not break the running gateway at all. In that narrow sense, no.


Phase 2 = take the request/response translator you built in Phase 1 and extend it so answers can flow back token-by-token in real time, correctly handling three different providers' streaming formats, client disconnects, and the fact that once you've started streaming you can't cleanly retry anymore.