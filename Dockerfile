FROM golang:1.25-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o eufy-server ./cmd/server

FROM alpine:3.21
# ffmpeg is required for on-the-fly H.265 -> H.264 transcoding so H.265
# cameras (SoloCam S340/E42 pan-tilt etc.) play in the web UI (MSE cannot
# decode HEVC). Recorders like Frigate can still consume the raw stream.
RUN apk add --no-cache ca-certificates ffmpeg
COPY --from=builder /build/eufy-server /usr/local/bin/eufy-server
EXPOSE 8080 8090
ENTRYPOINT ["eufy-server"]
CMD ["/config/config.toml"]
