# Build stage
FROM golang:1.27-alpine AS builder

WORKDIR /app
COPY go.mod *.go ./
RUN apk add --no-cache upx ca-certificates && apk upgrade --no-cache ca-certificates   # current CA bundle for the scratch image (LOCA-67: eMudhra roots)
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -trimpath -a -installsuffix cgo -o plex-clean . && \
    upx --best --lzma plex-clean || echo "UPX compression failed, continuing with uncompressed binary"

# Final stage - using scratch (empty) image instead of alpine
FROM scratch
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
WORKDIR /app
COPY --from=builder /app/plex-clean .
VOLUME /output /data
CMD ["/app/plex-clean"]
