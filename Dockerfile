# syntax=docker/dockerfile:1.10

FROM --platform=$BUILDPLATFORM golang:1.23-bookworm AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

COPY . .

ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w \
    -X github.com/snapp-incubator/snappcloud-bot/internal/version.Version=${VERSION} \
    -X github.com/snapp-incubator/snappcloud-bot/internal/version.Commit=${COMMIT} \
    -X github.com/snapp-incubator/snappcloud-bot/internal/version.Date=${DATE}" \
    -o /out/snappcloud-bot ./cmd/snappcloud-bot

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/snappcloud-bot /usr/local/bin/snappcloud-bot

USER nonroot:nonroot

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/snappcloud-bot"]
CMD ["-config=/etc/snappcloud-bot/config.yaml", "-addr=:8080"]
