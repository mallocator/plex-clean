# Build stage
FROM golang:1.24-alpine AS builder

WORKDIR /app
COPY go.mod *.go ./
RUN apk add --no-cache upx
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -trimpath -a -installsuffix cgo -o plex-clean . && \
    upx --best --lzma plex-clean || echo "UPX compression failed, continuing with uncompressed binary"

# Final stage - using scratch (empty) image instead of alpine
FROM scratch
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
WORKDIR /app
COPY --from=builder /app/plex-clean .
VOLUME /output
CMD ["/app/plex-clean"]
