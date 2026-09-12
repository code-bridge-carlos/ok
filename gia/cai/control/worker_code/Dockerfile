# worker-code (C6) — Build and run Go binary
# Worker superligero que solo implementa las tareas que C1 envía.
FROM golang:1.23-alpine AS builder
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /worker-code-linux .

FROM alpine:3.19
RUN apk add --no-cache ca-certificates git bash
COPY --from=builder /worker-code-linux /usr/local/bin/worker-code
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
EXPOSE 10000
ENTRYPOINT ["/entrypoint.sh"]
