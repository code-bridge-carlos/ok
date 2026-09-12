# C3 Carlos Code - Build and run Go binary
FROM golang:1.24-alpine AS builder
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN go mod tidy
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /carlos-code-linux .

FROM alpine:3.19
RUN apk add --no-cache ca-certificates curl
COPY --from=builder /carlos-code-linux /usr/local/bin/carlos-code
EXPOSE 8002
CMD ["carlos-code"]
