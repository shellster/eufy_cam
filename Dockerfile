FROM golang:1.25-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o eufy-server ./cmd/server

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY --from=builder /build/eufy-server /usr/local/bin/eufy-server
EXPOSE 8080 8090
ENTRYPOINT ["eufy-server"]
CMD ["/config/config.toml"]
