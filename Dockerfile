FROM golang:1.20-alpine AS builder

RUN apk update && apk add --no-cache make

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN make reqs-alpine
RUN make build

# stage 2
FROM alpine:latest

WORKDIR /app

COPY --from=builder /app/rtmp-auth /app/rtmp-auth

CMD ["/app/rtmp-auth"]
