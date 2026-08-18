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


The token bucket (the thing you have to write by hand)

This is the actual rate-limiting algorithm, and it's the classic whiteboard-interview question, which is why the plan won't let Claude Code write it for you.

The mental model: imagine a bucket that holds, say, 60 tokens. Every request costs 1 token to make. The bucket refills at a steady rate — say 1 token every second. If the bucket's empty, you're rate-limited; wait for it to refill. If it's full, you can burst — fire off 60 requests instantly if you want, then you're throttled to the refill rate after that.

Why this and not something simpler like "count requests in the last 60 seconds"? Because token bucket naturally allows short bursts (which real traffic actually looks like — nobody sends requests perfectly evenly spaced) while still enforcing a long-run average rate. That burst tolerance is the answer to the interview question "why token bucket over sliding window."

The tricky implementation detail: you're running this in Redis, and multiple copies of your gateway might be checking the same team's bucket at the same time. If two requests both read "5 tokens left," both think they're allowed, and both proceed — you've now let through 2 requests when only 1 should've fit. That's a race condition. The fix is a Lua script — a tiny script that Redis runs as a single, uninterruptible unit, so "check if there's a token, and if so, take it" happens as one atomic step nobody can sneak in the middle of.

"Lazy refill" instead of a background timer: rather than running a clock in the background that adds tokens every second (which gets messy across multiple server instances and can drift), you just store when the bucket was last touched. Next time someone checks the bucket, you calculate "how much time has passed since then, so how many tokens should've accrued" on the spot, right when it's needed. No background process, no synchronization headache, works identically whether you have 1 server or 10.

You've got two separate buckets per team — one for requests-per-minute, one for tokens-per-minute (an AI response with 4000 tokens should count differently than one with 40).

### What Phase 7 is actually building

Phase 6 built the logic of "if this fails, try somewhere else." But it had a hidden flaw: retry-and-fallback reacts to failures one at a time. If a provider is properly broken — not a blip, actually down — you'd still keep attempting it on every single new request, wait for it to time out, fail, then fall back. That's slow, and it keeps hammering something that's already struggling. The circuit breaker's whole job is to remember "this thing is currently broken" so you can stop even trying it until there's reason to believe it's recovered. This is the piece the plan flags as the single most likely thing an interviewer asks about, which is exactly why you're writing it by hand instead of letting Claude Code generate it.

Step 7.1 — The three-state light switch

Think of a circuit breaker like an actual electrical circuit breaker in your house — it exists to stop a small problem (one faulty appliance) from becoming a big one (the whole house catching fire). Three states:

Closed = normal, electricity flows, requests go through as usual. ("Closed" is confusing at first — it means the circuit is complete, so current is flowing. Don't read it as "closed for business.")
Open = tripped, the breaker is open, no current flows — every request gets rejected instantly, without even attempting to call the provider. You already know it's broken; why wait through another timeout to confirm what you already know?
HalfOpen = "let's cautiously test if it's safe again" — allow a small trickle of traffic through to check.

The transitions:

Closed → Open: too many failures happened recently (you're counting failures in a rolling recent window, not all-time — a provider that failed a lot yesterday but is fine now shouldn't still be penalized)
Open → HalfOpen: after waiting some cooldown period, automatically try being a bit more optimistic
HalfOpen → Closed: if a handful of test requests in a row succeed, trust it's actually recovered
HalfOpen → Open: if even one test request fails, nope — go straight back to fully blocking, and (smart addition) make the next cooldown even longer than before, so a provider that keeps failing gets tested less and less frequently instead of you hammering it every 30 seconds forever

## One limitation worth knowing before Phase 11

Chaos does not affect Provider.Ping. Health checks call the adapter directly, bypassing the proxy, so active checks keep succeeding while chaos is breaking real requests. The consequence for the 11.4 demo: a chaos'd provider goes degraded (via the passive error-rate signal) and its breaker opens and fallback engages — but it won't reach down via consecutive_ping_failures. That's arguably more realistic than the alternative, but it's a deliberate gap, not an oversight. Making Ping chaos-able would mean wrapping provider.Provider, which is a bigger structural change than 7.5 warrants — flag it if you'd rather have it.