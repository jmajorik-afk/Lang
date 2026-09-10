FROM golang:1.23 AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd
COPY pkg pkg
# go-sqlite3 needs cgo; link statically so the binary runs on plain alpine
RUN CGO_ENABLED=1 GOOS=linux go build -ldflags "-linkmode external -extldflags -static" -o langekko ./cmd

FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /app
COPY --from=builder /app/langekko .
COPY scripts scripts
COPY templates templates
# state lives in /app/data: mount it as a volume (SQLITE_PATH=./data/languagebot.db)
RUN mkdir -p data backups
VOLUME ["/app/data", "/app/backups"]
CMD ["./langekko", "telegram"]
