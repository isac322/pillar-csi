# syntax=docker/dockerfile:1

# Build the manager binary
FROM --platform=$BUILDPLATFORM public.ecr.aws/docker/library/golang:1.26-alpine3.23 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build \
      -trimpath \
      -pgo=auto \
      -ldflags="-s" \
      -o manager \
      cmd/main.go

FROM gcr.io/distroless/static:nonroot
COPY --from=builder --link /workspace/manager /usr/bin/manager
USER 65532:65532

ENTRYPOINT ["/usr/bin/manager"]
