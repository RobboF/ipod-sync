FROM golang:1.23-alpine AS builder
WORKDIR /src
COPY go.mod .
COPY main.go .
RUN go build -o /ipod-sync .

FROM alpine:latest
RUN apk add --no-cache dosfstools
COPY --from=builder /ipod-sync /usr/local/bin/ipod-sync
ENTRYPOINT ["/usr/local/bin/ipod-sync"]
