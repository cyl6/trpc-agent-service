# syntax=docker/dockerfile:1.7

FROM golang:1.24-alpine AS build

ARG TARGETOS=linux
ARG TARGETARCH

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" -o /out/trpc-service ./cmd/trpc-service

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 65532 trpc \
    && adduser -S -D -H -u 65532 -G trpc trpc

COPY --from=build /out/trpc-service /usr/local/bin/trpc-service
COPY deploy/service-config.yaml /etc/trpc-agent-service/config.yaml

USER 65532:65532
EXPOSE 8080

HEALTHCHECK --interval=10s --timeout=3s --start-period=10s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:8080/readyz || exit 1

ENTRYPOINT ["/usr/local/bin/trpc-service"]
CMD ["-config", "/etc/trpc-agent-service/config.yaml"]
