# Multi-stage build for cmd/gateway. The build stage has the full Go
# toolchain; the final stage is Alpine — small, but keeps a shell and curl
# so Compose can run a HEALTHCHECK against the container itself, plus CA
# certificates (needed for real HTTPS calls to OpenAI/Anthropic/Groq/Gemini)
# and configs/ as a runnable default that a bind mount can override.

FROM golang:1.26-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/
COPY migrations/ migrations/

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/migrate ./cmd/migrate

FROM alpine:3.20 AS final

RUN apk add --no-cache ca-certificates curl \
    && addgroup -S switchyard \
    && adduser -S -G switchyard switchyard

WORKDIR /app

COPY --from=build /out/gateway /app/gateway
COPY --from=build /out/migrate /app/migrate
COPY configs/ /app/configs/

EXPOSE 8080 9090

USER switchyard:switchyard

# /healthz, not /readyz: liveness only. A degraded or down provider must
# never get the gateway container itself restarted — that's exactly the
# "gateway is never the reason a request fails" rule, and restarting a
# perfectly healthy process because one upstream is having a bad day would
# violate it. /readyz (which does factor in provider health) is for a load
# balancer deciding whether to route traffic here, a different question
# from whether the container is alive.
HEALTHCHECK --interval=15s --timeout=3s --start-period=5s --retries=3 \
    CMD curl -f http://localhost:8080/healthz || exit 1

ENTRYPOINT ["/app/gateway"]
