FROM --platform=$BUILDPLATFORM golang:1.26.1-alpine AS builder

ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=dev

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/marstek-controller \
    ./cmd/marstek-controller

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/marstek-controller /marstek-controller
EXPOSE 8080
# Default to JSON logging so container stdout is Loki-ingestible out of the box.
# Override with LOG_FORMAT=text for local development.
ENV LOG_FORMAT=json
ENTRYPOINT ["/marstek-controller"]
