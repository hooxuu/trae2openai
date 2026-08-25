# ---- build stage: cross-compile on the build host's native arch ----
ARG GO_IMAGE=golang:1.24-alpine
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" -o /traeopenai .

# ---- runtime stage: minimal static image for the target platform ----
FROM scratch
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /traeopenai /traeopenai

ENV TRAE_LISTEN=0.0.0.0:8686 \
    TRAE_STATE_FILE=/data/state.json
VOLUME /data
EXPOSE 8686
ENTRYPOINT ["/traeopenai"]
